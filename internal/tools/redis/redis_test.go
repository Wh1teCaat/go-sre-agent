package redis

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
)

func TestRedisPingReturnsPongObservation(t *testing.T) {
	addr := startFakeRedis(t, "+PONG\r\n")

	tool := New()
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
	tool := New()

	_, err := tool.Run(context.Background(), mustArgs(t, Args{}))
	if err == nil {
		t.Fatal("expected missing addr to fail")
	}
}

func TestRedisPingSchemaDescribesOptionalPassword(t *testing.T) {
	schema := Spec().Schema

	if schema.Properties["password"].Type != "string" {
		t.Fatalf("password schema = %#v, want string", schema.Properties["password"])
	}
}

func startFakeRedis(t *testing.T, response string) string {
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
			if strings.TrimSpace(line) == "PING" {
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
