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

func TestRedisPingReturnsPongObservation(t *testing.T) {
	addr := startFakeRedis(t, "+PONG\r\n")

	tool := NewPing()
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{Addr: addr}))
	if err != nil {
		t.Fatalf("run redis ping: %v", err)
	}

	if observation.Tool != Name {
		t.Fatalf("tool = %q, want %q", observation.Tool, Name)
	}
	if observation.Data["addr"] != addr {
		t.Fatalf("addr = %#v, want %q", observation.Data["addr"], addr)
	}
	if observation.Data["response"] != "PONG" {
		t.Fatalf("response = %#v, want PONG", observation.Data["response"])
	}
	if !strings.Contains(observation.Summary, "PONG") {
		t.Fatalf("summary = %q, want PONG", observation.Summary)
	}
}

func TestRedisPingRejectsMissingAddr(t *testing.T) {
	tool := NewPing()

	_, err := tool.Run(context.Background(), mustArgs(t, Args{}))
	if err == nil {
		t.Fatal("expected missing addr to fail")
	}
}

func TestRedisPingSchemaDescribesOptionalPassword(t *testing.T) {
	schema := NewPing().Spec().Schema

	if schema.Properties["password"].Type != "string" {
		t.Fatalf("password schema = %#v, want string", schema.Properties["password"])
	}
}

func TestRedisCheckReportsInfoFields(t *testing.T) {
	info := "# Memory\r\nused_memory:1048576\r\nused_memory_human:1.00M\r\nmaxmemory:0\r\nmaxmemory_policy:noeviction\r\nevicted_keys:2\r\n# Clients\r\nconnected_clients:3\r\n# Keyspace\r\ndb0:keys=5,expires=1,avg_ttl=0\r\ndb1:keys=7,expires=0,avg_ttl=0\r\n"
	addr := startFakeRedisCommand(t, "INFO", fmt.Sprintf("$%d\r\n%s\r\n", len(info), info))

	observation, err := NewCheck().Run(context.Background(), mustCheckArgs(t, CheckArgs{Addr: addr}))
	if err != nil {
		t.Fatalf("run redis check: %v", err)
	}

	if observation.Tool != CheckName {
		t.Fatalf("tool = %q, want %q", observation.Tool, CheckName)
	}
	if observation.Data["keys"] != 12 {
		t.Fatalf("keys = %#v, want 12", observation.Data["keys"])
	}
	if observation.Data["evicted_keys"] != "2" {
		t.Fatalf("evicted_keys = %#v, want 2", observation.Data["evicted_keys"])
	}
	for _, want := range []string{"used_memory=1.00M", "policy=noeviction", "clients=3", "keys=12"} {
		if !strings.Contains(observation.Summary, want) {
			t.Fatalf("summary = %q, want %q", observation.Summary, want)
		}
	}
}

func TestRedisCheckReportsServerError(t *testing.T) {
	addr := startFakeRedisCommand(t, "INFO", "-NOAUTH Authentication required.\r\n")

	_, err := NewCheck().Run(context.Background(), mustCheckArgs(t, CheckArgs{Addr: addr}))
	if err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Fatalf("error = %v, want NOAUTH", err)
	}
}

func mustCheckArgs(t *testing.T, args CheckArgs) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}

func startFakeRedis(t *testing.T, response string) string {
	return startFakeRedisCommand(t, "PING", response)
}

func startFakeRedisCommand(t *testing.T, command string, response string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for i := 0; i < 5; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == command {
				break
			}
		}
		_, _ = conn.Write([]byte(response))
	}()

	return listener.Addr().String()
}

func mustArgs(t *testing.T, args Args) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}
