package websocket

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestWebSocketCheckReturnsSuccessfulHandshakeObservation(t *testing.T) {
	addr := startFakeWebSocketServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			t.Fatalf("path = %q, want /ws", r.URL.Path)
		}
		if r.Header.Get("Upgrade") != "websocket" {
			t.Fatalf("Upgrade = %q, want websocket", r.Header.Get("Upgrade"))
		}
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Sec-WebSocket-Accept", accept)
		w.WriteHeader(http.StatusSwitchingProtocols)
	})

	tool := NewWithAllowedHosts(nil)
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: "ws://" + addr + "/ws",
	}))
	if err != nil {
		t.Fatalf("run websocket check: %v", err)
	}

	if observation.Tool != Name {
		t.Fatalf("tool = %q, want %q", observation.Tool, Name)
	}
	if observation.Data["handshake_success"] != true {
		t.Fatalf("handshake_success = %#v, want true", observation.Data["handshake_success"])
	}
	if observation.Data["status"] != http.StatusSwitchingProtocols {
		t.Fatalf("status = %#v, want 101", observation.Data["status"])
	}
	if !strings.Contains(observation.Summary, "101") {
		t.Fatalf("summary = %q, want 101", observation.Summary)
	}
}

func TestWebSocketCheckReturnsFailedHandshakeObservationForHTTP500(t *testing.T) {
	addr := startFakeWebSocketServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"password=secret"}`))
	})

	tool := NewWithAllowedHosts(nil)
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: "ws://" + addr + "/ws",
	}))
	if err != nil {
		t.Fatalf("run websocket check: %v", err)
	}

	if observation.Data["handshake_success"] != false {
		t.Fatalf("handshake_success = %#v, want false", observation.Data["handshake_success"])
	}
	if observation.Data["status"] != http.StatusInternalServerError {
		t.Fatalf("status = %#v, want 500", observation.Data["status"])
	}
	if got := observation.Data["body_snippet"]; got != `{"error":"password=[REDACTED]"}` {
		t.Fatalf("body_snippet = %#v, want redacted body", got)
	}
}

func TestWriteHandshakeRejectsCRLFHeader(t *testing.T) {
	parsed, err := url.Parse("ws://localhost/ws")
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	client, _ := net.Pipe()
	defer client.Close()

	if err := writeHandshake(client, parsed, "key", map[string]string{"X-Test": "ok\r\nX-Injected: yes"}); err == nil {
		t.Fatal("expected CRLF header value to be rejected")
	}
}

func TestWebSocketCheckRejectsMissingURL(t *testing.T) {
	tool := NewWithAllowedHosts(nil)

	_, err := tool.Run(context.Background(), mustArgs(t, Args{}))
	if err == nil {
		t.Fatal("expected missing URL to fail")
	}
}

func TestWebSocketCheckRejectsDisallowedHostBeforeDial(t *testing.T) {
	tool := NewWithAllowedHosts([]string{"localhost"})

	_, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: "ws://example.com/ws",
	}))
	if err == nil {
		t.Fatal("expected disallowed host to fail")
	}
	if !strings.Contains(err.Error(), `host "example.com" is not allowed`) {
		t.Fatalf("error = %q, want disallowed host", err.Error())
	}
}

func TestWebSocketCheckSchemaDescribesOptionalHeaders(t *testing.T) {
	schema := Spec().Schema

	if schema.Properties["headers"].Type != "object" {
		t.Fatalf("headers schema = %#v, want object", schema.Properties["headers"])
	}
}

func startFakeWebSocketServer(t *testing.T, handler http.HandlerFunc) string {
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
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		recorder := &rawResponseWriter{
			conn:   conn,
			header: make(http.Header),
		}
		handler(recorder, request)
		recorder.flush()
	}()

	return listener.Addr().String()
}

type rawResponseWriter struct {
	conn        net.Conn
	header      http.Header
	status      int
	wroteHeader bool
	body        strings.Builder
}

func (w *rawResponseWriter) Header() http.Header {
	return w.header
}

func (w *rawResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(data)
}

func (w *rawResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.status = statusCode
	w.wroteHeader = true
}

func (w *rawResponseWriter) flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	reason := http.StatusText(w.status)
	if reason == "" {
		reason = "status"
	}
	_, _ = fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", w.status, reason)
	for key, values := range w.header {
		for _, value := range values {
			_, _ = fmt.Fprintf(w.conn, "%s: %s\r\n", key, value)
		}
	}
	if w.body.Len() > 0 {
		_, _ = fmt.Fprintf(w.conn, "Content-Length: %d\r\n", w.body.Len())
	}
	_, _ = fmt.Fprint(w.conn, "\r\n")
	if w.body.Len() > 0 {
		_, _ = fmt.Fprint(w.conn, w.body.String())
	}
}

func mustArgs(t *testing.T, args Args) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}
