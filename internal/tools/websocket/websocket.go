package websocket

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const Name = "websocket_check"

type Args struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Ping    bool              `json:"ping,omitempty"`
}

type Tool struct {
	allowedHosts tools.AllowedHosts
}

func NewWithAllowedHosts(allowedHosts []string) *Tool {
	return &Tool{allowedHosts: tools.NewAllowedHosts(allowedHosts)}
}

func (t *Tool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Attempt a WebSocket connection and report handshake success or failure.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"url":     {Type: "string", Required: true, Description: "WebSocket URL to dial."},
				"headers": {Type: "object", Description: "Optional headers for the WebSocket handshake."},
				"ping":    {Type: "boolean", Description: "After a successful handshake, send a protocol ping frame and verify the pong reply to check the message path."},
			},
		},
	}
}

func (t *Tool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args Args
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode websocket_check args: %w", err)
	}
	args.URL = strings.TrimSpace(args.URL)
	if args.URL == "" {
		return schema.Observation{}, fmt.Errorf("websocket_check requires url")
	}

	parsed, err := url.Parse(args.URL)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("parse websocket url: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return schema.Observation{}, fmt.Errorf("websocket url scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return schema.Observation{}, fmt.Errorf("websocket url requires host")
	}
	if !t.allowedHosts.Allows(parsed.Hostname()) {
		// WebSocket 检查同样会拨真实网络连接，必须在 dial 前执行主机 allowlist。
		return schema.Observation{}, tools.DisallowedHostError(parsed.Hostname())
	}

	addr := websocketAddr(parsed)
	key, err := websocketKey()
	if err != nil {
		return schema.Observation{}, fmt.Errorf("generate websocket key: %w", err)
	}

	startedAt := time.Now()
	conn, err := dial(ctx, parsed, addr)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("connect websocket: %w", err)
	}
	defer conn.Close()
	tools.ApplyConnDeadline(ctx, conn)

	if err := writeHandshake(conn, parsed, key, args.Headers); err != nil {
		return schema.Observation{}, fmt.Errorf("write websocket handshake: %w", err)
	}
	// 握手后的数据帧必须复用同一个 reader，否则缓冲里的帧字节会丢失。
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("read websocket handshake response: %w", err)
	}
	defer response.Body.Close()

	latencyMS := time.Since(startedAt).Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}

	bodySnippet := ""
	if response.StatusCode != http.StatusSwitchingProtocols {
		// 非 101 也不是工具错误：HTTP 状态码和响应片段正是排查握手失败的证据。
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		bodySnippet = tools.RedactSensitive(string(body))
		return schema.Observation{
			Tool:    Name,
			Summary: fmt.Sprintf("WebSocket %s handshake returned %d in %dms", args.URL, response.StatusCode, latencyMS),
			Data: map[string]any{
				"url":               args.URL,
				"status":            response.StatusCode,
				"handshake_success": false,
				"latency_ms":        latencyMS,
				"body_snippet":      bodySnippet,
			},
		}, nil
	}

	expectedAccept := websocketAccept(key)
	actualAccept := response.Header.Get("Sec-WebSocket-Accept")
	// 101 只能说明服务端同意升级；Sec-WebSocket-Accept 必须按 RFC 6455
	// 与客户端 key 匹配，才能确认握手不是普通 HTTP 响应伪装出来的。
	success := strings.EqualFold(response.Header.Get("Upgrade"), "websocket") &&
		strings.EqualFold(actualAccept, expectedAccept)

	summary := fmt.Sprintf("WebSocket %s handshake returned 101 in %dms", args.URL, latencyMS)
	if !success {
		summary = fmt.Sprintf("WebSocket %s handshake returned 101 but validation failed in %dms", args.URL, latencyMS)
	}
	data := map[string]any{
		"url":                   args.URL,
		"status":                response.StatusCode,
		"handshake_success":     success,
		"latency_ms":            latencyMS,
		"sec_websocket_accept":  actualAccept,
		"expected_accept_match": success,
	}

	if args.Ping && success {
		// ping/pong 失败不是工具错误：握手成功但消息通路不通本身就是关键证据。
		pongLatencyMS, err := verifyPingPong(conn, reader)
		if err != nil {
			data["ping_pong_ok"] = false
			data["ping_error"] = tools.RedactSensitive(err.Error())
			summary += "; ping/pong failed"
		} else {
			data["ping_pong_ok"] = true
			data["pong_latency_ms"] = pongLatencyMS
			summary += fmt.Sprintf("; ping/pong verified in %dms", pongLatencyMS)
		}
	}

	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data:    data,
	}, nil
}

// verifyPingPong 在握手成功后发送一帧协议层 ping 并等待 pong，
// 验证消息通路是否可用，而不发送任何业务数据帧。
func verifyPingPong(conn net.Conn, reader *bufio.Reader) (int64, error) {
	startedAt := time.Now()
	if err := writePingFrame(conn); err != nil {
		return 0, fmt.Errorf("write ping frame: %w", err)
	}
	// 服务端可能先推送欢迎或业务帧；跳过有限个非 pong 帧后再判失败。
	for range 8 {
		opcode, payloadLen, err := readFrameHeader(reader)
		if err != nil {
			return 0, fmt.Errorf("read frame: %w", err)
		}
		if err := discardFramePayload(reader, payloadLen); err != nil {
			return 0, fmt.Errorf("read frame payload: %w", err)
		}
		switch opcode {
		case 0xA: // pong 帧
			latency := time.Since(startedAt).Milliseconds()
			if latency < 0 {
				latency = 0
			}
			return latency, nil
		case 0x8: // close 帧
			return 0, fmt.Errorf("server closed connection before pong")
		}
	}
	return 0, fmt.Errorf("no pong within first 8 frames")
}

func writePingFrame(conn net.Conn) error {
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	payload := []byte("ping")
	frame := make([]byte, 0, 2+4+len(payload))
	// FIN|ping opcode；客户端帧必须置 mask 位并用 4 字节掩码异或负载（RFC 6455）。
	frame = append(frame, 0x89, byte(0x80|len(payload)))
	frame = append(frame, key[:]...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}
	_, err := conn.Write(frame)
	return err
}

func readFrameHeader(reader *bufio.Reader) (byte, int64, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, 0, err
	}
	opcode := header[0] & 0x0F
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, 0, err
		}
		length = int64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(reader, extended); err != nil {
			return 0, 0, err
		}
		length = int64(binary.BigEndian.Uint64(extended))
	}
	if masked {
		if _, err := io.CopyN(io.Discard, reader, 4); err != nil {
			return 0, 0, err
		}
	}
	return opcode, length, nil
}

func discardFramePayload(reader *bufio.Reader, length int64) error {
	const maxFrameBytes = 1 << 20
	if length < 0 || length > maxFrameBytes {
		return fmt.Errorf("frame payload length %d exceeds limit", length)
	}
	_, err := io.CopyN(io.Discard, reader, length)
	return err
}

func dial(ctx context.Context, parsed *url.URL, addr string) (net.Conn, error) {
	if parsed.Scheme == "wss" {
		tlsDialer := &tls.Dialer{
			NetDialer: new(net.Dialer),
			Config: &tls.Config{
				ServerName: parsed.Hostname(),
				MinVersion: tls.VersionTLS12,
			},
		}
		return tlsDialer.DialContext(ctx, "tcp", addr)
	}
	return new(net.Dialer).DialContext(ctx, "tcp", addr)
}

func websocketAddr(parsed *url.URL) string {
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

func websocketKey() (string, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func writeHandshake(conn net.Conn, parsed *url.URL, key string, headers map[string]string) error {
	requestURI := parsed.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\n", requestURI)
	fmt.Fprintf(&b, "Host: %s\r\n", parsed.Host)
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, value := range headers {
		if isProtectedHeader(name) {
			// 保护协议必需头，避免调用方覆盖 Host/Upgrade/Key 导致检查语义失真。
			continue
		}
		if strings.ContainsAny(name, "\r\n:") || strings.TrimSpace(name) == "" {
			return fmt.Errorf("invalid websocket header name")
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("invalid websocket header value for %q", name)
		}
		fmt.Fprintf(&b, "%s: %s\r\n", name, value)
	}
	b.WriteString("\r\n")

	_, err := io.WriteString(conn, b.String())
	return err
}

func isProtectedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "host", "upgrade", "connection", "sec-websocket-key", "sec-websocket-version":
		return true
	default:
		return false
	}
}

func websocketAccept(key string) string {
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(hash[:])
}
