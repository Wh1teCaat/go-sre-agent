package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
)

// startFakeRedisScan 响应一轮 SCAN 和后续 TTL 查询。
func startFakeRedisScan(t *testing.T, keys []string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			command := strings.TrimSpace(line)
			switch {
			case command == "SCAN":
				var b strings.Builder
				fmt.Fprintf(&b, "*2\r\n$1\r\n0\r\n*%d\r\n", len(keys))
				for _, key := range keys {
					fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(key), key)
				}
				_, _ = conn.Write([]byte(b.String()))
			case command == "TTL":
				_, _ = conn.Write([]byte(":117\r\n"))
			}
		}
	}()
	return listener.Addr().String()
}

func TestRedisScanListsKeysWithTTL(t *testing.T) {
	addr := startFakeRedisScan(t, []string{"presence:user:1:connections", "presence:user:2:connections"})
	tool := NewScan([]string{"presence:"})

	observation, err := tool.Run(context.Background(), mustScanArgs(t, ScanArgs{Addr: addr, Pattern: "presence:*"}))
	if err != nil {
		t.Fatalf("run redis scan: %v", err)
	}

	if observation.Data["key_count"] != 2 {
		t.Fatalf("key_count = %#v, want 2", observation.Data["key_count"])
	}
	entries := observation.Data["keys"].([]map[string]any)
	if entries[0]["key"] != "presence:user:1:connections" || entries[0]["ttl_seconds"] != int64(117) {
		t.Fatalf("entries = %#v", entries)
	}
	if !strings.Contains(observation.Summary, `2 keys matching "presence:*"`) {
		t.Fatalf("summary = %q", observation.Summary)
	}
}

func TestRedisScanRejectsPatternOutsidePrefixAllowlist(t *testing.T) {
	tool := NewScan([]string{"presence:"})

	_, err := tool.Run(context.Background(), mustScanArgs(t, ScanArgs{Addr: "localhost:6379", Pattern: "message:*"}))
	if err == nil || !strings.Contains(err.Error(), "does not match any allowed prefix") {
		t.Fatalf("error = %v, want prefix rejection", err)
	}
}

func mustScanArgs(t *testing.T, args ScanArgs) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}
