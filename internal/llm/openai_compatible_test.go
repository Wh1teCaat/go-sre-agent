package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOpenAICompatibleConfigReadsDotEnv(t *testing.T) {
	unsetLLMConfigEnv(t)
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(`
OPENAI_API_KEY=
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_MODEL=gpt-4o-mini
SRE_AGENT_LLM_PROVIDER=openai_compatible
`), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", cfg.Model)
	}
	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("base url = %q", cfg.BaseURL)
	}
	if cfg.Provider != "openai_compatible" {
		t.Fatalf("provider = %q, want openai_compatible", cfg.Provider)
	}
}

func unsetLLMConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"SRE_AGENT_LLM_PROVIDER",
		"MIMO_API_KEY", "MIMO_BASE_URL", "MIMO_MODEL",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_MODEL",
		"OLLAMA_BASE_URL", "OLLAMA_MODEL",
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
	} {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func TestLoadOpenAICompatibleConfigDefaultsModelToGPT4OMini(t *testing.T) {
	clearOpenAIEnv(t)

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", cfg.Model)
	}
	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("base url = %q", cfg.BaseURL)
	}
}

func TestOpenAICompatibleChatClientSendsChatRequestAndParsesContent(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotRequest openAIChatCompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request body: %v\n%s", err, body)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [
				{
					"message": {
						"content": "{\"type\":\"final\",\"thought_summary\":\"smoke complete\",\"final\":{\"summary\":\"LLM provider is wired\"}}"
					}
				}
			]
		}`))
	}))
	defer server.Close()

	client := NewOpenAICompatibleChatClient(Config{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Model:   "gpt-4o-mini",
	})

	response, err := client.Chat(context.Background(), ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []Message{
			{Role: RoleSystem, Content: "Return JSON."},
			{Role: RoleUser, Content: "只做一次 LLM provider 烟测"},
		},
		OutputMode:  OutputJSON,
		Temperature: 0.2,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization header = %q, want bearer token", gotAuth)
	}
	if gotRequest.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", gotRequest.Model)
	}
	if len(gotRequest.Messages) != 2 {
		t.Fatalf("messages length = %d, want 2", len(gotRequest.Messages))
	}
	if gotRequest.Messages[0].Role != "system" {
		t.Fatalf("first message role = %q, want system", gotRequest.Messages[0].Role)
	}
	if !strings.Contains(gotRequest.Messages[0].Content, "JSON") {
		t.Fatalf("system prompt must mention JSON: %q", gotRequest.Messages[0].Content)
	}
	if gotRequest.ResponseFormat == nil {
		t.Fatal("response format = nil, want json_object")
	}
	if gotRequest.ResponseFormat.Type != "json_object" {
		t.Fatalf("response format = %q, want json_object", gotRequest.ResponseFormat.Type)
	}
	if !strings.Contains(response, "LLM provider is wired") {
		t.Fatalf("response content = %q, want raw model content", response)
	}
}

func TestOpenAICompatibleChatClientReturnsErrorForNon2xxResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad api key", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewOpenAICompatibleChatClient(Config{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Model:   "gpt-4o-mini",
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Model:       "gpt-4o-mini",
		Messages:    []Message{{Role: RoleUser, Content: "诊断"}},
		OutputMode:  OutputJSON,
		Temperature: 0.2,
	})
	if err == nil {
		t.Fatal("chat error = nil, want non-2xx error")
	}
	if !strings.Contains(err.Error(), "status 401") {
		t.Fatalf("error = %q, want status 401", err.Error())
	}
}

func TestOpenAICompatibleChatClientRequiresAPIKey(t *testing.T) {
	client := NewOpenAICompatibleChatClient(Config{
		BaseURL: "https://api.openai.com/v1",
		Model:   "gpt-4o-mini",
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []Message{{Role: RoleUser, Content: "诊断"}},
	})
	if err == nil {
		t.Fatal("chat error = nil, want missing api key error")
	}
	if !strings.Contains(err.Error(), "api key") {
		t.Fatalf("error = %q, want api key", err.Error())
	}
}

func clearOpenAIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("MIMO_API_KEY", "")
	t.Setenv("MIMO_BASE_URL", "")
	t.Setenv("MIMO_MODEL", "")
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv("OLLAMA_MODEL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "")
}

func TestConfigSelectsMIMOAndProviderSpecificValues(t *testing.T) {
	mimo := ConfigFromValues(map[string]string{
		"SRE_AGENT_LLM_PROVIDER": "openai_compatible",
		"MIMO_API_KEY":           "mimo-key",
		"MIMO_BASE_URL":          "https://mimo.example/v1",
		"OPENAI_API_KEY":         "openai-key",
	})
	if mimo.APIKey != "mimo-key" || mimo.BaseURL != "https://mimo.example/v1" {
		t.Fatalf("mimo config = %#v", mimo)
	}

	ollama := ConfigFromValues(map[string]string{
		"SRE_AGENT_LLM_PROVIDER": "ollama",
		"OLLAMA_MODEL":           "qwen3:8b",
	})
	if ollama.APIKey != "ollama" || ollama.BaseURL != DefaultOllamaBaseURL || ollama.Model != "qwen3:8b" {
		t.Fatalf("ollama config = %#v", ollama)
	}

	anthropic := ConfigFromValues(map[string]string{
		"SRE_AGENT_LLM_PROVIDER": "anthropic",
		"ANTHROPIC_API_KEY":      "claude-key",
		"ANTHROPIC_MODEL":        "claude-test",
	})
	if anthropic.APIKey != "claude-key" || anthropic.BaseURL != DefaultAnthropicBaseURL || anthropic.Model != "claude-test" {
		t.Fatalf("anthropic config = %#v", anthropic)
	}
}
