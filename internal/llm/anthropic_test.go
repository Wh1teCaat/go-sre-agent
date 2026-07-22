package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAnthropicChatClientMapsMessagesAndParsesText(t *testing.T) {
	var gotRequest anthropicMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("path = %q, want /messages", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != anthropicVersion {
			t.Fatalf("anthropic headers are missing")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"type\":\"final\"}"}]}`))
	}))
	defer server.Close()

	client := NewAnthropicChatClient(Config{APIKey: "test-key", BaseURL: server.URL, Model: "claude-test"})
	response, err := client.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "只返回 JSON"},
			{Role: RoleUser, Content: "诊断服务"},
		},
		OutputMode:  OutputJSON,
		Temperature: 0.2,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotRequest.Model != "claude-test" || gotRequest.System != "只返回 JSON" {
		t.Fatalf("request = %#v", gotRequest)
	}
	if len(gotRequest.Messages) != 1 || gotRequest.Messages[0].Role != "user" {
		t.Fatalf("messages = %#v", gotRequest.Messages)
	}
	if response != `{"type":"final"}` {
		t.Fatalf("content = %q", response)
	}
}

func TestAnthropicChatClientRequiresCredentialsAndModel(t *testing.T) {
	request := ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hello"}}}
	if _, err := NewAnthropicChatClient(Config{Model: "claude-test"}).Chat(context.Background(), request); err == nil {
		t.Fatal("chat succeeded without api key")
	}
	if _, err := NewAnthropicChatClient(Config{APIKey: "test-key"}).Chat(context.Background(), request); err == nil {
		t.Fatal("chat succeeded without model")
	}
}
