package websocket

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
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
}

type Tool struct {
	dialer       *net.Dialer
	allowedHosts tools.AllowedHosts
}

func NewWithAllowedHosts(dialer *net.Dialer, allowedHosts []string) *Tool {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return &Tool{
		dialer:       dialer,
		allowedHosts: tools.NewAllowedHosts(allowedHosts),
	}
}

func (t *Tool) Name() string {
	return Name
}

func (t *Tool) Description() string {
	return Spec().Description
}

func (t *Tool) Schema() tools.ToolSchema {
	return Spec().Schema
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
	conn, err := t.dial(ctx, parsed, addr)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("connect websocket: %w", err)
	}
	defer conn.Close()
	applyConnDeadline(ctx, conn)

	if err := writeHandshake(conn, parsed, key, args.Headers); err != nil {
		return schema.Observation{}, fmt.Errorf("write websocket handshake: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
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
		bodySnippet = string(body)
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

	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data: map[string]any{
			"url":                   args.URL,
			"status":                response.StatusCode,
			"handshake_success":     success,
			"latency_ms":            latencyMS,
			"sec_websocket_accept":  actualAccept,
			"expected_accept_match": success,
		},
	}, nil
}

func (t *Tool) dial(ctx context.Context, parsed *url.URL, addr string) (net.Conn, error) {
	if parsed.Scheme == "wss" {
		tlsDialer := &tls.Dialer{
			NetDialer: t.dialer,
			Config: &tls.Config{
				ServerName: parsed.Hostname(),
				MinVersion: tls.VersionTLS12,
			},
		}
		return tlsDialer.DialContext(ctx, "tcp", addr)
	}
	return t.dialer.DialContext(ctx, "tcp", addr)
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

func applyConnDeadline(ctx context.Context, conn net.Conn) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
}

func Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Attempt a WebSocket connection and report handshake success or failure.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"url":     {Type: "string", Required: true, Description: "WebSocket URL to dial."},
				"headers": {Type: "object", Description: "Optional headers for the WebSocket handshake."},
			},
		},
	}
}
