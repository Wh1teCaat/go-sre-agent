package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const CheckName = "redis_check"

// maxInfoBytes 限制 INFO 响应体大小，异常服务端不能把内存打满。
const maxInfoBytes = 256 * 1024

type CheckArgs struct {
	Addr     string `json:"addr"`
	Password string `json:"password,omitempty"`
}

type CheckTool struct{}

func NewCheck() *CheckTool { return &CheckTool{} }

func (t *CheckTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        CheckName,
		Description: "Read Redis INFO and report memory usage, maxmemory policy, evictions, client counts, and keyspace size.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"addr":     {Type: "string", Required: true, Description: "Redis address such as localhost:6379."},
				"password": {Type: "string", Description: "Optional Redis AUTH password."},
			},
		},
	}
}

func (t *CheckTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args CheckArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode redis_check args: %w", err)
	}
	args.Addr = strings.TrimSpace(args.Addr)
	if args.Addr == "" {
		return schema.Observation{}, fmt.Errorf("redis_check requires addr")
	}

	startedAt := time.Now()
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", args.Addr)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("connect redis: %w", err)
	}
	defer conn.Close()
	tools.ApplyConnDeadline(ctx, conn)

	reader := bufio.NewReader(conn)
	if args.Password != "" {
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

	if err := writeRedisCommand(conn, "INFO"); err != nil {
		return schema.Observation{}, fmt.Errorf("send redis INFO: %w", err)
	}
	info, err := readRedisBulkString(reader)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("read redis INFO response: %w", err)
	}

	fields, totalKeys := parseRedisInfo(info)
	latencyMS := time.Since(startedAt).Milliseconds()
	if latencyMS < 0 {
		latencyMS = 0
	}

	data := map[string]any{
		"addr":       args.Addr,
		"latency_ms": latencyMS,
		"keys":       totalKeys,
	}
	// run_id/os/版本是实例身份指纹：区分容器内 Redis 与端口上可能冒名的其他实例。
	for _, field := range []string{
		"used_memory", "used_memory_human", "maxmemory", "maxmemory_human", "maxmemory_policy",
		"evicted_keys", "connected_clients", "blocked_clients", "rejected_connections",
		"redis_version", "run_id", "os", "uptime_in_seconds",
	} {
		if value, ok := fields[field]; ok {
			data[field] = value
		}
	}

	return schema.Observation{
		Tool: CheckName,
		Summary: fmt.Sprintf(
			"Redis %s INFO: used_memory=%s maxmemory=%s policy=%s evicted_keys=%s clients=%s keys=%d in %dms",
			args.Addr,
			fieldOr(fields, "used_memory_human", "used_memory"),
			fieldOr(fields, "maxmemory_human", "maxmemory"),
			fieldOr(fields, "maxmemory_policy"),
			fieldOr(fields, "evicted_keys"),
			fieldOr(fields, "connected_clients"),
			totalKeys,
			latencyMS,
		),
		Data: data,
	}, nil
}

func fieldOr(fields map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := fields[key]; value != "" {
			return value
		}
	}
	return "unknown"
}

// readRedisBulkString 读取 RESP bulk string（$<len>\r\n<payload>\r\n），INFO 使用该编码。
func readRedisBulkString(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.HasPrefix(line, "-") {
		return "", fmt.Errorf("%s", strings.TrimPrefix(line, "-"))
	}
	if !strings.HasPrefix(line, "$") {
		return "", fmt.Errorf("unexpected redis response %q", line)
	}
	length, err := strconv.Atoi(strings.TrimPrefix(line, "$"))
	if err != nil {
		return "", fmt.Errorf("invalid redis bulk length %q", line)
	}
	if length < 0 {
		return "", nil
	}
	if length > maxInfoBytes {
		return "", fmt.Errorf("redis INFO response exceeds %d bytes", maxInfoBytes)
	}
	payload := make([]byte, length+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", err
	}
	return string(payload[:length]), nil
}

// parseRedisInfo 解析 INFO 的 key:value 行，并把 dbN keyspace 行汇总为总 key 数。
func parseRedisInfo(info string) (map[string]string, int) {
	fields := map[string]string{}
	totalKeys := 0
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[key] = value
		// keyspace 行形如 db0:keys=5,expires=0,avg_ttl=0
		if strings.HasPrefix(key, "db") {
			for _, part := range strings.Split(value, ",") {
				if count, found := strings.CutPrefix(part, "keys="); found {
					if parsed, err := strconv.Atoi(count); err == nil {
						totalKeys += parsed
					}
				}
			}
		}
	}
	return fields, totalKeys
}
