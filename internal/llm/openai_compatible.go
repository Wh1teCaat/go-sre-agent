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

// OpenAICompatibleChatClient 把项目的通用 ChatClient 契约映射到
// OpenAI-compatible /chat/completions HTTP API。它不理解 SRE action，
// action 规划仍然是 ActionPlanner 的职责。
type OpenAICompatibleChatClient struct {
	config Config
	client *http.Client
}

func NewOpenAICompatibleChatClient(config Config) *OpenAICompatibleChatClient {
	if config.Provider == "" {
		config.Provider = "openai_compatible"
	}
	if config.BaseURL == "" {
		config.BaseURL = DefaultOpenAICompatibleBaseURL
	}
	if config.Model == "" {
		config.Model = DefaultOpenAICompatibleModel
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	return &OpenAICompatibleChatClient{
		config: config,
		client: http.DefaultClient,
	}
}

// Chat 把通用 chat 请求发送到 OpenAI-compatible endpoint，并返回原始
// assistant content 供上层解释。
func (c *OpenAICompatibleChatClient) Chat(ctx context.Context, request ChatRequest) (ChatResponse, error) {
	if strings.TrimSpace(c.config.APIKey) == "" {
		return ChatResponse{}, fmt.Errorf("openai compatible api key is required")
	}

	payload := buildOpenAIChatCompletionRequest(c.config, request)
	body, err := json.Marshal(payload)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("encode openai compatible request: %w", err)
	}

	// 所有厂商差异都压在 /chat/completions 兼容层里；上层只看到 ChatClient。
	// BaseURL 在构造 client 时已经去掉尾部斜杠，这里可以直接拼路径。
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("create openai compatible request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")

	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("call openai compatible chat completions: %w", err)
	}
	defer response.Body.Close()

	// 限制响应体大小，避免错误页或异常代理响应把内存打满。
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read openai compatible response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ChatResponse{}, fmt.Errorf("openai compatible request failed: status %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var completion openAIChatCompletionResponse
	if err := json.Unmarshal(responseBody, &completion); err != nil {
		return ChatResponse{}, fmt.Errorf("decode openai compatible response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("openai compatible response has no choices")
	}

	content := strings.TrimSpace(completion.Choices[0].Message.Content)
	if content == "" {
		return ChatResponse{}, fmt.Errorf("openai compatible response content is empty")
	}
	return ChatResponse{Content: content}, nil
}

type openAIChatCompletionRequest struct {
	Model          string                `json:"model"`
	Messages       []openAIChatMessage   `json:"messages"`
	ResponseFormat *openAIResponseFormat `json:"response_format,omitempty"`
	Temperature    float64               `json:"temperature"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFormat struct {
	Type string `json:"type"`
}

type openAIChatCompletionResponse struct {
	Choices []openAIChatChoice `json:"choices"`
}

type openAIChatChoice struct {
	Message openAIChatMessage `json:"message"`
}

func buildOpenAIChatCompletionRequest(config Config, request ChatRequest) openAIChatCompletionRequest {
	model := request.Model
	if strings.TrimSpace(model) == "" {
		model = config.Model
	}

	messages := make([]openAIChatMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, openAIChatMessage{
			Role:    string(message.Role),
			Content: message.Content,
		})
	}

	payload := openAIChatCompletionRequest{
		Model:       model,
		Messages:    messages,
		Temperature: request.Temperature,
	}
	if request.OutputMode == OutputJSON {
		// JSON mode 只提高模型输出 JSON 的概率，不能替代 ActionPlanner 和 policy 的解析/校验。
		payload.ResponseFormat = &openAIResponseFormat{Type: "json_object"}
	}
	return payload
}
