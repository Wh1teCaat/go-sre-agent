package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const Name = "redis_ping"

type Args struct {
	Addr     string `json:"addr"`
	Password string `json:"password,omitempty"`
}

type Tool struct {
	dialer *net.Dialer
}

func New(dialer *net.Dialer) *Tool {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return &Tool{dialer: dialer}
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
		return schema.Observation{}, fmt.Errorf("decode redis_ping args: %w", err)
	}
	args.Addr = strings.TrimSpace(args.Addr)
	if args.Addr == "" {
		return schema.Observation{}, fmt.Errorf("redis_ping requires addr")
	}

	startedAt := time.Now()
	conn, err := t.dialer.DialContext(ctx, "tcp", args.Addr)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("connect redis: %w", err)
	}
	defer conn.Close()
	applyConnDeadline(ctx, conn)

	reader := bufio.NewReader(conn)
	if args.Password != "" {
		// AUTH 是可选前置步骤；失败时直接返回错误，避免把未授权的 PING 当成网络故障。
		if err := writeRedisCommand(conn, "AUTH", args.Password); err != nil {
			return schema.Observation{}, fmt.Errorf("send redis AUTH: %w", err)
		}
		response, err := readRedisSimpleLine(reader)
		if err != nil {
			return schema.Observation{}, fmt.Errorf("read redis AUTH response: %w", err)
		}
		if response != "OK" {
			return schema.Observation{}, fmt.Errorf("redis AUTH returned %q", response)
		}
	}

	if err := writeRedisCommand(conn, "PING"); err != nil {
		return schema.Observation{}, fmt.Errorf("send redis PING: %w", err)
	}
	response, err := readRedisSimpleLine(reader)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("read redis PING response: %w", err)
	}
	if response != "PONG" {
		return schema.Observation{}, fmt.Errorf("redis PING returned %q", response)
	}

	latencyMS := time.Since(startedAt).Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}
	return schema.Observation{
		Tool:    Name,
		Summary: fmt.Sprintf("Redis %s responded PONG in %dms", args.Addr, latencyMS),
		Data: map[string]any{
			"addr":       args.Addr,
			"response":   response,
			"latency_ms": latencyMS,
		},
	}, nil
}

func writeRedisCommand(conn net.Conn, parts ...string) error {
	var b strings.Builder
	// Redis 使用 RESP array 表示命令：*参数个数，随后每个参数用 bulk string 编码。
	b.WriteString(fmt.Sprintf("*%d\r\n", len(parts)))
	for _, part := range parts {
		b.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(part), part))
	}
	_, err := conn.Write([]byte(b.String()))
	return err
}

func readRedisSimpleLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.HasPrefix(line, "-") {
		// RESP error 以 - 开头，保留服务端错误文本给上层 observation。
		return "", fmt.Errorf("%s", strings.TrimPrefix(line, "-"))
	}
	if strings.HasPrefix(line, "+") {
		return strings.TrimPrefix(line, "+"), nil
	}
	return "", fmt.Errorf("unexpected redis response %q", line)
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
		Description: "Connect to Redis and run PING.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"addr":     {Type: "string", Required: true, Description: "Redis address such as localhost:6379."},
				"password": {Type: "string", Description: "Optional Redis AUTH password."},
			},
		},
	}
}
