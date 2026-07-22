package postgres

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
)

func TestPostgresPingSendsStartupAndReturnsServerResponse(t *testing.T) {
	addr := startFakePostgres(t, func(conn net.Conn) {
		lengthBytes := make([]byte, 4)
		if _, err := io.ReadFull(conn, lengthBytes); err != nil {
			t.Errorf("read startup length: %v", err)
			return
		}
		length := binary.BigEndian.Uint32(lengthBytes)
		if length < 8 {
			t.Errorf("startup length = %d, want at least 8", length)
			return
		}
		payload := make([]byte, int(length)-4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			t.Errorf("read startup payload: %v", err)
			return
		}
		if !strings.Contains(string(payload), "application_name") {
			t.Errorf("startup payload missing application_name: %q", string(payload))
			return
		}
		_, _ = conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0})
	})

	tool := NewPing()
	observation, err := tool.Run(context.Background(), mustPostgresArgs(t, PingArgs{
		DSN: "postgres://app:secret@" + addr + "/chat_proj?sslmode=disable",
	}))
	if err != nil {
		t.Fatalf("run postgres ping: %v", err)
	}

	if observation.Tool != PingName {
		t.Fatalf("tool = %q, want %q", observation.Tool, PingName)
	}
	if observation.Data["addr"] != addr {
		t.Fatalf("addr = %#v, want %q", observation.Data["addr"], addr)
	}
	if observation.Data["server_response"] != "authentication_ok" {
		t.Fatalf("server_response = %#v, want authentication_ok", observation.Data["server_response"])
	}
}

func TestPostgresPingRejectsMissingDSN(t *testing.T) {
	tool := NewPing()

	_, err := tool.Run(context.Background(), mustPostgresArgs(t, PingArgs{}))
	if err == nil {
		t.Fatal("expected missing dsn to fail")
	}
}

func startFakePostgres(t *testing.T, handler func(net.Conn)) string {
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
		handler(conn)
	}()

	return listener.Addr().String()
}

func mustPostgresArgs(t *testing.T, args PingArgs) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}
