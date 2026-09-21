package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const ScanName = "redis_scan"

const (
	// maxScanKeys 限制返回键数，防止大 keyspace 塞爆 observation。
	maxScanKeys = 50
	// maxScanIterations 限制 SCAN 游标轮数，防止超大库上无限扫描。
	maxScanIterations = 100
)

type ScanArgs struct {
	Addr     string `json:"addr"`
	Password string `json:"password,omitempty"`
	Pattern  string `json:"pattern"`
}

// ScanTool 按前缀白名单做键级只读查询（SCAN + TTL）。
// 白名单避免把业务数据（如消息缓存）整体暴露进 LLM 上下文。
type ScanTool struct {
	allowedPrefixes []string
}

func NewScan(allowedPrefixes []string) *ScanTool {
	cleaned := make([]string, 0, len(allowedPrefixes))
	for _, prefix := range allowedPrefixes {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			cleaned = append(cleaned, prefix)
		}
	}
	sort.Strings(cleaned)
	return &ScanTool{allowedPrefixes: cleaned}
}

func (t *ScanTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: ScanName,
		Description: "Read-only Redis key inspection: SCAN keys matching an allowed prefix and report each key's TTL. " +
			"Use to verify server-side state such as presence or rate-limit entries. Allowed prefixes: " +
			strings.Join(t.allowedPrefixes, ", "),
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"addr":     {Type: "string", Required: true, Description: "Redis address such as localhost:6379."},
				"password": {Type: "string", Description: "Optional Redis AUTH password."},
				"pattern":  {Type: "string", Required: true, Description: "Key pattern starting with an allowed prefix, for example presence:*."},
			},
		},
	}
}

func (t *ScanTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args ScanArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode redis_scan args: %w", err)
	}
	args.Addr = strings.TrimSpace(args.Addr)
	if args.Addr == "" {
		return schema.Observation{}, fmt.Errorf("redis_scan requires addr")
	}
	pattern := strings.TrimSpace(args.Pattern)
	if pattern == "" {
		return schema.Observation{}, fmt.Errorf("redis_scan requires pattern")
	}
	if !t.patternAllowed(pattern) {
		return schema.Observation{}, fmt.Errorf("pattern %q does not match any allowed prefix (%s)", pattern, strings.Join(t.allowedPrefixes, ", "))
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

	keys, truncated, err := scanKeys(conn, reader, pattern)
	if err != nil {
		return schema.Observation{}, err
	}

	entries := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		if err := writeRedisCommand(conn, "TTL", key); err != nil {
			return schema.Observation{}, fmt.Errorf("send redis TTL: %w", err)
		}
		ttl, err := readRedisInteger(reader)
		if err != nil {
			return schema.Observation{}, fmt.Errorf("read redis TTL for %q: %w", key, err)
		}
		entries = append(entries, map[string]any{"key": key, "ttl_seconds": ttl})
	}

	latencyMS := time.Since(startedAt).Milliseconds()
	summary := fmt.Sprintf("Redis %s has %d keys matching %q in %dms", args.Addr, len(keys), pattern, latencyMS)
	if truncated {
		summary += fmt.Sprintf(" (truncated at %d)", maxScanKeys)
	}
	return schema.Observation{
		Tool:    ScanName,
		Summary: summary,
		Data: map[string]any{
			"addr":       args.Addr,
			"pattern":    pattern,
			"keys":       entries,
			"key_count":  len(keys),
			"truncated":  truncated,
			"latency_ms": latencyMS,
		},
	}, nil
}

func (t *ScanTool) patternAllowed(pattern string) bool {
	for _, prefix := range t.allowedPrefixes {
		if strings.HasPrefix(pattern, prefix) {
			return true
		}
	}
	return false
}

func scanKeys(conn net.Conn, reader *bufio.Reader, pattern string) ([]string, bool, error) {
	cursor := "0"
	keys := []string{}
	for iteration := 0; iteration < maxScanIterations; iteration++ {
		if err := writeRedisCommand(conn, "SCAN", cursor, "MATCH", pattern, "COUNT", "100"); err != nil {
			return nil, false, fmt.Errorf("send redis SCAN: %w", err)
		}
		next, batch, err := readScanReply(reader)
		if err != nil {
			return nil, false, fmt.Errorf("read redis SCAN reply: %w", err)
		}
		keys = append(keys, batch...)
		if len(keys) >= maxScanKeys {
			return keys[:maxScanKeys], true, nil
		}
		if next == "0" {
			return keys, false, nil
		}
		cursor = next
	}
	return keys, true, nil
}

// readScanReply 解析 SCAN 的两元素数组回复：[next-cursor, [key...]]。
func readScanReply(reader *bufio.Reader) (string, []string, error) {
	header, err := readLine(reader)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return "", nil, fmt.Errorf("unexpected SCAN reply %q", header)
	}
	cursor, err := readBulkString(reader)
	if err != nil {
		return "", nil, err
	}
	countLine, err := readLine(reader)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(countLine, "*") {
		return "", nil, fmt.Errorf("unexpected SCAN keys reply %q", countLine)
	}
	count, err := strconv.Atoi(strings.TrimPrefix(countLine, "*"))
	if err != nil || count < 0 || count > 10000 {
		return "", nil, fmt.Errorf("invalid SCAN keys count %q", countLine)
	}
	keys := make([]string, 0, count)
	for range count {
		key, err := readBulkString(reader)
		if err != nil {
			return "", nil, err
		}
		keys = append(keys, key)
	}
	return cursor, keys, nil
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.HasPrefix(line, "-") {
		return "", fmt.Errorf("%s", strings.TrimPrefix(line, "-"))
	}
	return line, nil
}

func readBulkString(reader *bufio.Reader) (string, error) {
	header, err := readLine(reader)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(header, "$") {
		return "", fmt.Errorf("unexpected bulk string header %q", header)
	}
	length, err := strconv.Atoi(strings.TrimPrefix(header, "$"))
	if err != nil || length < 0 || length > 1<<20 {
		return "", fmt.Errorf("invalid bulk string length %q", header)
	}
	payload := make([]byte, length+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return "", err
	}
	return string(payload[:length]), nil
}

func readRedisInteger(reader *bufio.Reader) (int64, error) {
	line, err := readLine(reader)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(line, ":") {
		return 0, fmt.Errorf("unexpected integer reply %q", line)
	}
	return strconv.ParseInt(strings.TrimPrefix(line, ":"), 10, 64)
}
