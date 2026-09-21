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
		w.Header().Set("X-Request-ID", "req-test")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from chat_proj backend"))
	}))
	defer server.Close()

	tool := NewWithPolicy(12, nil, nil)
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
	if observation.Data["request_id"] != "req-test" {
		t.Fatalf("request_id = %#v, want req-test", observation.Data["request_id"])
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

	tool := NewWithPolicy(64, nil, nil)
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

	tool := NewWithPolicy(256, nil, nil)
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
	tool := NewWithPolicy(64, nil, nil)

	_, err := tool.Run(context.Background(), mustArgs(t, Args{}))
	if err == nil {
		t.Fatal("expected missing URL to fail")
	}
}

func TestHTTPCheckRejectsDisallowedHostBeforeRequest(t *testing.T) {
	tool := NewWithPolicy(64, []string{"localhost"}, nil)

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

	tool := NewWithPolicy(64, nil, []string{server.URL + "/v1/user/login"})
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
	tool := NewWithPolicy(64, []string{parsed.Hostname()}, nil)
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

func TestHTTPCheckRepeatSamplesFlakyEndpoint(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tool := NewWithPolicy(64, nil, nil)
	observation, err := tool.Run(context.Background(), mustArgs(t, Args{URL: server.URL, Repeat: 4}))
	if err != nil {
		t.Fatalf("run http check: %v", err)
	}

	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	counts, ok := observation.Data["status_counts"].(map[string]int)
	if !ok || counts["200"] != 2 || counts["500"] != 2 {
		t.Fatalf("status_counts = %#v, want 200x2 and 500x2", observation.Data["status_counts"])
	}
	if !strings.Contains(observation.Summary, "sampled 4 times") || !strings.Contains(observation.Summary, "200x2, 500x2") {
		t.Fatalf("summary = %q, want sampling summary", observation.Summary)
	}
	if observation.Data["attempts"] != 4 {
		t.Fatalf("attempts = %#v, want 4", observation.Data["attempts"])
	}
}

func TestHTTPCheckSchemaDescribesOptionalHeadersAndBody(t *testing.T) {
	schema := NewWithPolicy(0, nil, nil).Spec().Schema

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
