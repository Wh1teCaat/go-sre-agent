package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
)

func TestRunDiagnoseLogin500ScenarioExecutesHTTPAndLogTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/user/login" {
			t.Fatalf("path = %q, want /v1/user/login", r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("login failed"))
	}))
	defer server.Close()

	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "chat_proj.log")
	if err := os.WriteFile(logFile, []byte("INFO start\nERROR login failed: pq: relation users does not exist\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	markdown, err := runDiagnose(context.Background(), diagnoseOptions{
		Goal:           "诊断登录 500",
		MockScenario:   "login-500",
		BackendBaseURL: server.URL,
		LogFile:        logFile,
		AllowedLogDir:  logDir,
		MaxSteps:       3,
		ToolTimeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	for _, want := range []string{
		"登录接口返回 500",
		"Step 1 `http_check`",
		"Step 2 `log_read`",
		`read 1 log lines matching "ERROR"`,
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunDiagnoseDependencyCheckScenarioExecutesPostgresAndRedisTools(t *testing.T) {
	postgresAddr := startFakePostgres(t)
	redisAddr := startFakeRedis(t)

	markdown, err := runDiagnose(context.Background(), diagnoseOptions{
		Goal:         "检查依赖",
		MockScenario: "dependency-check",
		PostgresDSN:  "postgres://app:secret@" + postgresAddr + "/chat_proj?sslmode=disable",
		RedisAddr:    redisAddr,
		MaxSteps:     3,
		ToolTimeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	for _, want := range []string{
		"依赖连通性检查完成",
		"Step 1 `postgres_ping`",
		"Step 2 `redis_ping`",
		"authentication_ok",
		"responded PONG",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunDiagnoseWebSocketScenarioExecutesWebSocketAndLogTools(t *testing.T) {
	wsAddr := startFakeWebSocket(t, http.StatusInternalServerError)
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "chat_proj.log")
	if err := os.WriteFile(logFile, []byte("INFO start\nERROR websocket upgrade failed: missing Authorization header\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	markdown, err := runDiagnose(context.Background(), diagnoseOptions{
		Goal:          "诊断 WebSocket",
		MockScenario:  "websocket",
		WebSocketURL:  "ws://" + wsAddr + "/ws",
		LogFile:       logFile,
		AllowedLogDir: logDir,
		MaxSteps:      3,
		ToolTimeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	for _, want := range []string{
		"WebSocket 诊断链路已完成",
		"Step 1 `websocket_check`",
		"Step 2 `log_read`",
		"handshake returned 500",
		`read 1 log lines matching "websocket"`,
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunDiagnoseUsesOpenAICompatibleChatClientFromEnvWhenNoMockScenario(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	var gotRequest openAIChatCompletionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [
				{
					"message": {
						"content": "{\"type\":\"final\",\"thought_summary\":\"smoke complete\",\"final\":{\"summary\":\"真实 provider 已接入\"}}"
					}
				}
			]
		}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	markdown, err := runDiagnose(context.Background(), diagnoseOptions{
		Goal:           "只做一次 provider 接入烟测",
		BackendBaseURL: "http://chat-proj.local:8080",
		LogFile:        "/var/log/chat_proj/app.log",
		PostgresDSN:    "postgres://app:super-secret@db.local:5432/chat_proj?sslmode=disable",
		RedisAddr:      "redis.local:6379",
		WebSocketURL:   "ws://chat-proj.local:8080/ws",
		MaxSteps:       1,
		ToolTimeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	if !strings.Contains(markdown, "真实 provider 已接入") {
		t.Fatalf("markdown missing real provider result:\n%s", markdown)
	}
	if len(gotRequest.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(gotRequest.Messages))
	}
	contextMessage := gotRequest.Messages[1].Content
	for _, want := range []string{
		"target_context",
		"backend_base_url",
		"http://chat-proj.local:8080",
		"login_url",
		"/v1/user/login",
		"postgres_dsn",
		"db.local:5432",
		"redis.local:6379",
		"ws://chat-proj.local:8080/ws",
		"/var/log/chat_proj/app.log",
	} {
		if !strings.Contains(contextMessage, want) {
			t.Fatalf("model context missing %q:\n%s", want, contextMessage)
		}
	}
	if strings.Contains(contextMessage, "super-secret") {
		t.Fatalf("model context leaked postgres password:\n%s", contextMessage)
	}
}

func TestRunDiagnoseRealProviderCanDriveToolLoop(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	backendHits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits++
		if r.URL.Path != "/health" {
			t.Fatalf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer backend.Close()

	var gotRequests []openAIChatCompletionRequestForTest
	llmCalls := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var gotRequest openAIChatCompletionRequestForTest
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}
		gotRequests = append(gotRequests, gotRequest)

		content := ""
		switch llmCalls {
		case 1:
			content = fmt.Sprintf(`{"type":"tool_call","thought_summary":"check backend health","tool":"http_check","args":{"url":%q}}`, backend.URL+"/health")
		case 2:
			content = `{"type":"final","thought_summary":"backend evidence is enough","final":{"summary":"LLM tool loop complete","evidence":[{"step":1,"tool":"http_check","summary":"backend returned ok"}]}}`
		default:
			t.Fatalf("unexpected llm call %d", llmCalls)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]string{
						"content": content,
					},
				},
			},
		}); err != nil {
			t.Fatalf("write llm response: %v", err)
		}
	}))
	defer llmServer.Close()
	t.Setenv("OPENAI_BASE_URL", llmServer.URL)

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	markdown, err := runDiagnose(context.Background(), diagnoseOptions{
		Goal:           "检查后端是否存活",
		BackendBaseURL: backend.URL,
		AllowedHosts:   []string{backendURL.Hostname()},
		MaxSteps:       2,
		ToolTimeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	if backendHits != 1 {
		t.Fatalf("backend hits = %d, want 1", backendHits)
	}
	if llmCalls != 2 {
		t.Fatalf("llm calls = %d, want 2", llmCalls)
	}
	if len(gotRequests) != 2 {
		t.Fatalf("captured llm requests = %d, want 2", len(gotRequests))
	}
	if !strings.Contains(gotRequests[0].Messages[1].Content, "target_context") {
		t.Fatalf("first request missing target context:\n%s", gotRequests[0].Messages[1].Content)
	}
	secondContext := gotRequests[1].Messages[1].Content
	for _, want := range []string{"http_check", "returned 200", `"status": 200`} {
		if !strings.Contains(secondContext, want) {
			t.Fatalf("second request missing %q:\n%s", want, secondContext)
		}
	}
	for _, want := range []string{"LLM tool loop complete", "Step 1 `http_check`", "returned 200"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunLLMChatReturnsRawModelContent(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	var gotRequest openAIChatCompletionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [
				{
					"message": {
						"content": "pong"
					}
				}
			]
		}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	content, err := runLLMChat(context.Background(), llmChatOptions{
		Message: "ping",
	})
	if err != nil {
		t.Fatalf("run llm chat: %v", err)
	}

	if content != "pong" {
		t.Fatalf("content = %q, want pong", content)
	}
	if gotRequest.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", gotRequest.Model)
	}
	if gotRequest.ResponseFormat != nil {
		t.Fatalf("response_format = %#v, want nil for raw chat", gotRequest.ResponseFormat)
	}
	if len(gotRequest.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(gotRequest.Messages))
	}
	if gotRequest.Messages[0].Role != string(llm.RoleSystem) {
		t.Fatalf("first role = %q, want system", gotRequest.Messages[0].Role)
	}
	if gotRequest.Messages[1].Content != "ping" {
		t.Fatalf("user message = %q, want ping", gotRequest.Messages[1].Content)
	}
}

func TestRunLLMPingUsesDefaultPingMessage(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	var gotRequest openAIChatCompletionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	content, err := runLLMPing(context.Background())
	if err != nil {
		t.Fatalf("run llm ping: %v", err)
	}

	if content != "ok" {
		t.Fatalf("content = %q, want ok", content)
	}
	if len(gotRequest.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(gotRequest.Messages))
	}
	if !strings.Contains(gotRequest.Messages[1].Content, "pong") {
		t.Fatalf("ping message = %q, want pong instruction", gotRequest.Messages[1].Content)
	}
}

func TestResolveDiagnoseOptionsLoadsConfigAndAppliesOverrides(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
agent:
  max_steps: 11
  tool_timeout: 9s
policy:
  tool_allowlist:
    - http_check
    - log_read
  allowed_log_dirs:
    - /configured/logs
  allowed_hosts:
    - configured.local
    - localhost
targets:
  backend_base_url: http://configured:8080
  postgres_dsn: postgres://configured:secret@db:5432/chat_proj?sslmode=disable
  redis_addr: redis:6379
  websocket_url: ws://configured/ws
  log_file: /configured/logs/app.log
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts, err := resolveDiagnoseOptions(diagnoseOptions{
		Goal:           "diagnose",
		ConfigPath:     configPath,
		BackendBaseURL: "http://override:8080",
	})
	if err != nil {
		t.Fatalf("resolve options: %v", err)
	}

	if opts.BackendBaseURL != "http://override:8080" {
		t.Fatalf("backend base URL = %q, want override", opts.BackendBaseURL)
	}
	if opts.LogFile != "/configured/logs/app.log" {
		t.Fatalf("log file = %q, want config value", opts.LogFile)
	}
	if opts.AllowedLogDir != "/configured/logs" {
		t.Fatalf("allowed log dir = %q, want config value", opts.AllowedLogDir)
	}
	if len(opts.AllowedHosts) != 2 || opts.AllowedHosts[0] != "configured.local" || opts.AllowedHosts[1] != "localhost" {
		t.Fatalf("allowed hosts = %#v, want configured.local/localhost", opts.AllowedHosts)
	}
	if opts.PostgresDSN != "postgres://configured:secret@db:5432/chat_proj?sslmode=disable" {
		t.Fatalf("postgres dsn = %q, want config value", opts.PostgresDSN)
	}
	if opts.RedisAddr != "redis:6379" {
		t.Fatalf("redis addr = %q, want config value", opts.RedisAddr)
	}
	if opts.WebSocketURL != "ws://configured/ws" {
		t.Fatalf("websocket URL = %q, want config value", opts.WebSocketURL)
	}
	if opts.MaxSteps != 11 {
		t.Fatalf("max steps = %d, want 11", opts.MaxSteps)
	}
	if opts.ToolTimeout != 9*time.Second {
		t.Fatalf("tool timeout = %s, want 9s", opts.ToolTimeout)
	}
}

type openAIChatCompletionRequestForTest struct {
	Model          string                       `json:"model"`
	Messages       []openAIChatMessageForTest   `json:"messages"`
	ResponseFormat *openAIResponseFormatForTest `json:"response_format,omitempty"`
}

type openAIChatMessageForTest struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFormatForTest struct {
	Type string `json:"type"`
}

func clearLLMEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
}

func startFakePostgres(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen postgres: %v", err)
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

		lengthBytes := make([]byte, 4)
		if _, err := io.ReadFull(conn, lengthBytes); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(lengthBytes)
		payload := make([]byte, int(length)-4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		_, _ = conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0})
	}()

	return listener.Addr().String()
}

func startFakeWebSocket(t *testing.T, status int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen websocket: %v", err)
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
		_, err = http.ReadRequest(reader)
		if err != nil {
			return
		}
		reason := http.StatusText(status)
		if reason == "" {
			reason = "status"
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 14\r\n\r\nupgrade failed", status, reason)
	}()

	return listener.Addr().String()
}

func startFakeRedis(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen redis: %v", err)
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
		_, _ = conn.Write([]byte("+PONG\r\n"))
	}()

	return listener.Addr().String()
}
