package httpcheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPCheckReturnsStatusLatencyAndBodySnippet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from chat_proj backend"))
	}))
	defer server.Close()

	tool := New(nil, 12)
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: server.URL,
	}))
	if err != nil {
		t.Fatalf("run http check: %v", err)
	}

	if observation.Tool != Name {
		t.Fatalf("tool = %q, want %q", observation.Tool, Name)
	}
	if observation.Data["status"] != http.StatusOK {
		t.Fatalf("status = %#v, want %d", observation.Data["status"], http.StatusOK)
	}
	if observation.Data["body_snippet"] != "hello from c" {
		t.Fatalf("body snippet = %#v, want %q", observation.Data["body_snippet"], "hello from c")
	}
	if latency, ok := observation.Data["latency_ms"].(int64); !ok || latency < 0 {
		t.Fatalf("latency_ms = %#v, want non-negative int64", observation.Data["latency_ms"])
	}
}

func TestHTTPCheckDoesNotTreatHTTP500AsToolError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("database exploded"))
	}))
	defer server.Close()

	tool := New(nil, 64)
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: server.URL,
	}))
	if err != nil {
		t.Fatalf("run http check: %v", err)
	}

	if observation.Data["status"] != http.StatusInternalServerError {
		t.Fatalf("status = %#v, want %d", observation.Data["status"], http.StatusInternalServerError)
	}
	if !strings.Contains(observation.Summary, "500") {
		t.Fatalf("summary = %q, want status code", observation.Summary)
	}
}

func TestHTTPCheckRedactsSensitiveBodySnippet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"password":"super-secret","token":"abc123","message":"ok"}`))
	}))
	defer server.Close()

	tool := New(nil, 256)
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: server.URL,
	}))
	if err != nil {
		t.Fatalf("run http check: %v", err)
	}

	snippet := observation.Data["body_snippet"].(string)
	for _, leaked := range []string{"super-secret", "abc123"} {
		if strings.Contains(snippet, leaked) {
			t.Fatalf("body snippet leaked %q: %s", leaked, snippet)
		}
	}
	if count := strings.Count(snippet, "[REDACTED]"); count != 2 {
		t.Fatalf("body snippet redaction count = %d, want 2: %s", count, snippet)
	}
}

func TestHTTPCheckRejectsMissingURL(t *testing.T) {
	tool := New(nil, 64)

	_, err := tool.Run(context.Background(), mustArgs(t, Args{}))
	if err == nil {
		t.Fatal("expected missing URL to fail")
	}
}

func TestHTTPCheckRejectsDisallowedHostBeforeRequest(t *testing.T) {
	tool := NewWithAllowedHosts(nil, 64, []string{"localhost"})

	_, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL: "http://example.com/health",
	}))
	if err == nil {
		t.Fatal("expected disallowed host to fail")
	}
	if !strings.Contains(err.Error(), `host "example.com" is not allowed`) {
		t.Fatalf("error = %q, want disallowed host", err.Error())
	}
}

func TestHTTPCheckAllowsOnlyConfiguredPOSTURL(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tool := NewWithPolicy(nil, 64, nil, []string{server.URL + "/v1/user/login"})
	if _, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL:    server.URL + "/v1/user/login",
		Method: http.MethodPost,
	})); err != nil {
		t.Fatalf("configured login POST failed: %v", err)
	}
	_, err := tool.Run(context.Background(), mustArgs(t, Args{
		URL:    server.URL + "/v1/user/delete",
		Method: http.MethodDelete,
	}))
	if err == nil || !strings.Contains(err.Error(), `http method "DELETE" is not allowed`) {
		t.Fatalf("error = %v, want unsafe method error", err)
	}
	_, err = tool.Run(context.Background(), mustArgs(t, Args{
		URL:    server.URL + "/v1/user/create",
		Method: http.MethodPost,
	}))
	if err == nil || !strings.Contains(err.Error(), "POST url") {
		t.Fatalf("error = %v, want unconfigured POST error", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want only configured POST to reach server", requests)
	}
}

func TestHTTPCheckRejectsRedirectToDisallowedHost(t *testing.T) {
	targetReached := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			target, err := url.Parse(serverURLWithHost(server.URL, "localhost") + "/target")
			if err != nil {
				t.Fatalf("parse redirect target: %v", err)
			}
			http.Redirect(w, r, target.String(), http.StatusFound)
			return
		}
		targetReached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	tool := NewWithAllowedHosts(nil, 64, []string{parsed.Hostname()})
	_, err = tool.Run(context.Background(), mustArgs(t, Args{URL: server.URL + "/redirect"}))
	if err == nil || !strings.Contains(err.Error(), `host "localhost" is not allowed`) {
		t.Fatalf("error = %v, want disallowed redirect host", err)
	}
	if targetReached {
		t.Fatal("redirect target was reached")
	}
}

func serverURLWithHost(rawURL string, host string) string {
	parsed, _ := url.Parse(rawURL)
	parsed.Host = host + ":" + parsed.Port()
	return parsed.String()
}

func TestHTTPCheckSchemaDescribesOptionalHeadersAndBody(t *testing.T) {
	schema := Spec().Schema

	if schema.Properties["headers"].Type != "object" {
		t.Fatalf("headers schema = %#v, want object", schema.Properties["headers"])
	}
	if schema.Properties["body"].Type != "string" {
		t.Fatalf("body schema = %#v, want string", schema.Properties["body"])
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
