package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const PingName = "postgres_ping"

type PingArgs struct {
	DSN string `json:"dsn"`
}

type PingTool struct{}

type endpoint struct {
	addr     string
	user     string
	database string
}

func NewPing() *PingTool { return &PingTool{} }

func (t *PingTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        PingName,
		Description: "Open a PostgreSQL connection and perform a lightweight startup-message reachability check.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"dsn": {Type: "string", Required: true, Description: "PostgreSQL connection string."},
			},
		},
	}
}

func (t *PingTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args PingArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode postgres_ping args: %w", err)
	}
	target, err := parseEndpoint(args.DSN)
	if err != nil {
		return schema.Observation{}, err
	}

	startedAt := time.Now()
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", target.addr)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("connect postgres: %w", err)
	}
	defer conn.Close()
	tools.ApplyConnDeadline(ctx, conn)

	// 这里不引入完整 PostgreSQL 驱动，也不执行 SQL。
	// 启动消息足够验证 TCP 可达、协议响应和认证阶段是否能推进。
	if err := sendStartupMessage(conn, target); err != nil {
		return schema.Observation{}, fmt.Errorf("send postgres startup message: %w", err)
	}
	response, err := readServerResponse(conn)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("read postgres response: %w", err)
	}

	latencyMS := time.Since(startedAt).Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}
	return schema.Observation{
		Tool:    PingName,
		Summary: fmt.Sprintf("PostgreSQL %s responded with %s in %dms", target.addr, response, latencyMS),
		Data: map[string]any{
			"addr":            target.addr,
			"database":        target.database,
			"user":            target.user,
			"server_response": response,
			"latency_ms":      latencyMS,
			"check":           "startup_message",
		},
	}, nil
}

func parseEndpoint(dsn string) (endpoint, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return endpoint{}, fmt.Errorf("postgres_ping requires dsn")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		// URL 格式 DSN 是应用常见配置形式，例如 postgres://user:pass@host/db。
		return parseURLEndpoint(dsn)
	}
	// 同时支持 libpq keyword 格式，便于直接复用已有环境变量。
	return parseKeywordEndpoint(dsn)
}

func parseURLEndpoint(dsn string) (endpoint, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return endpoint{}, fmt.Errorf("parse postgres dsn: %w", err)
	}
	host := parsed.Hostname()
	if host == "" {
		return endpoint{}, fmt.Errorf("postgres dsn requires host")
	}
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	user := "postgres"
	if parsed.User != nil && parsed.User.Username() != "" {
		user = parsed.User.Username()
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" {
		database = user
	}
	return endpoint{
		addr:     net.JoinHostPort(host, port),
		user:     user,
		database: database,
	}, nil
}

func parseKeywordEndpoint(dsn string) (endpoint, error) {
	values := map[string]string{}
	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return endpoint{}, fmt.Errorf("invalid postgres keyword dsn field %q", field)
		}
		values[key] = strings.Trim(value, "'\"")
	}
	host := values["host"]
	if host == "" {
		host = "localhost"
	}
	port := values["port"]
	if port == "" {
		port = "5432"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return endpoint{}, fmt.Errorf("invalid postgres port %q", port)
	}
	user := values["user"]
	if user == "" {
		user = "postgres"
	}
	database := values["dbname"]
	if database == "" {
		database = user
	}
	return endpoint{
		addr:     net.JoinHostPort(host, port),
		user:     user,
		database: database,
	}, nil
}

func sendStartupMessage(conn net.Conn, target endpoint) error {
	var payload bytes.Buffer
	// 196608 是 PostgreSQL protocol version 3.0。后面跟若干
	// null-terminated key/value 参数，并以额外的 0 结束。
	if err := binary.Write(&payload, binary.BigEndian, uint32(196608)); err != nil {
		return err
	}
	writeCString(&payload, "user")
	writeCString(&payload, target.user)
	writeCString(&payload, "database")
	writeCString(&payload, target.database)
	writeCString(&payload, "application_name")
	writeCString(&payload, "go-sre-agent")
	payload.WriteByte(0)

	var packet bytes.Buffer
	length := uint32(payload.Len() + 4)
	if err := binary.Write(&packet, binary.BigEndian, length); err != nil {
		return err
	}
	packet.Write(payload.Bytes())
	_, err := conn.Write(packet.Bytes())
	return err
}

func writeCString(buffer *bytes.Buffer, value string) {
	buffer.WriteString(value)
	buffer.WriteByte(0)
}

func readServerResponse(conn net.Conn) (string, error) {
	messageType := make([]byte, 1)
	if _, err := io.ReadFull(conn, messageType); err != nil {
		return "", err
	}
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(conn, lengthBytes); err != nil {
		return "", err
	}
	length := binary.BigEndian.Uint32(lengthBytes)
	if length < 4 {
		return "", fmt.Errorf("invalid postgres message length %d", length)
	}
	payload := make([]byte, int(length)-4)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return "", err
	}

	switch messageType[0] {
	case 'R':
		// AuthenticationOk 表示协议层和目标库/用户至少已被服务端接受到认证阶段。
		// 非 0 认证码也说明服务端可达，只是需要密码/SASL 等后续认证。
		if len(payload) < 4 {
			return "", fmt.Errorf("authentication message too short")
		}
		authCode := binary.BigEndian.Uint32(payload[:4])
		if authCode == 0 {
			return "authentication_ok", nil
		}
		return fmt.Sprintf("authentication_required:%d", authCode), nil
	case 'E':
		return "error_response", nil
	default:
		return fmt.Sprintf("message:%c", messageType[0]), nil
	}
}
