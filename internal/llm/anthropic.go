package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const anthropicVersion = "2023-06-01"

// AnthropicChatClient 把通用 ChatClient 请求映射到 Anthropic Messages API。
type AnthropicChatClient struct {
	config Config
	client *http.Client
}

func NewAnthropicChatClient(config Config) *AnthropicChatClient {
	if config.BaseURL == "" {
		config.BaseURL = DefaultAnthropicBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	return &AnthropicChatClient{config: config, client: http.DefaultClient}
}

func (c *AnthropicChatClient) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	if strings.TrimSpace(c.config.APIKey) == "" {
		return ChatResponse{}, fmt.Errorf("anthropic api key is required")
	}
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = strings.TrimSpace(c.config.Model)
	}
	if model == "" {
		return ChatResponse{}, fmt.Errorf("anthropic model is required")
	}

	payload, err := buildAnthropicMessageRequest(model, request)
	if err != nil {
		return ChatResponse{}, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("encode anthropic request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("create anthropic request: %w", err)
	}
	httpRequest.Header.Set("x-api-key", c.config.APIKey)
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
	httpRequest.Header.Set("Content-Type", "application/json")

	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("call anthropic messages: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read anthropic response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ChatResponse{}, fmt.Errorf("anthropic request failed: status %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var message anthropicMessageResponse
	if err := json.Unmarshal(responseBody, &message); err != nil {
		return ChatResponse{}, fmt.Errorf("decode anthropic response: %w", err)
	}
	parts := make([]string, 0, len(message.Content))
	for _, block := range message.Content {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, strings.TrimSpace(block.Text))
		}
	}
	content := strings.Join(parts, "\n")
	if content == "" {
		return ChatResponse{}, fmt.Errorf("anthropic response has no text content")
	}
	return ChatResponse{Content: content}, nil
}

type anthropicMessageRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature float64            `json:"temperature"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicMessageResponse struct {
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func buildAnthropicMessageRequest(model string, request ChatRequest) (anthropicMessageRequest, error) {
	systems := make([]string, 0, 1)
	messages := make([]anthropicMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		switch message.Role {
		case RoleSystem:
			systems = append(systems, message.Content)
		case RoleUser, RoleAssistant:
			messages = append(messages, anthropicMessage{Role: string(message.Role), Content: message.Content})
		default:
			return anthropicMessageRequest{}, fmt.Errorf("anthropic does not support message role %q", message.Role)
		}
	}
	if len(messages) == 0 {
		return anthropicMessageRequest{}, fmt.Errorf("anthropic request requires at least one user or assistant message")
	}
	return anthropicMessageRequest{
		Model:       model,
		MaxTokens:   2048,
		System:      strings.Join(systems, "\n\n"),
		Messages:    messages,
		Temperature: request.Temperature,
	}, nil
}
