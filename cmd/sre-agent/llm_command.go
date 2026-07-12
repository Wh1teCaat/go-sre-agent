package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/y2/go-sre-agent/internal/llm"
)

func pingLLM(ctx context.Context) (string, error) {
	return chatWithLLM(ctx, llmChatOptions{
		Message: "这是 LLM 连通性测试。请只回复 pong。",
	})
}

// chatWithLLM 只验证底层 ChatClient 连通性，直接返回模型原始文本。
// 诊断链路仍然通过 ActionPlanner 把模型输出解析成结构化 action。
func chatWithLLM(ctx context.Context, opts llmChatOptions) (string, error) {
	message := strings.TrimSpace(opts.Message)
	if message == "" {
		return "", fmt.Errorf("message is required")
	}

	llmConfig, err := loadLLMConfig()
	if err != nil {
		return "", err
	}
	client, err := newChatClient(llmConfig)
	if err != nil {
		return "", err
	}

	response, err := client.Chat(ctx, llm.ChatRequest{
		Model: llmConfig.Model,
		Messages: []llm.Message{
			{
				Role:    llm.RoleSystem,
				Content: "你是 go-sre-agent 的 LLM 连通性测试助手。直接、简洁地回答用户消息。",
			},
			{
				Role:    llm.RoleUser,
				Content: message,
			},
		},
		OutputMode:  llm.OutputText,
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	return response.Content, nil
}

// loadLLMConfig 优先读取当前目录 .env，找不到时回退到进程环境变量。
func loadLLMConfig() (llm.Config, error) {
	const envPath = ".env"
	if _, err := os.Stat(envPath); err == nil {
		return llm.LoadConfig(envPath)
	} else if err != nil && !os.IsNotExist(err) {
		return llm.Config{}, fmt.Errorf("stat env file: %w", err)
	}
	return llm.LoadConfig("")
}

// newChatClient 是 CLI 唯一的模型协议选择点；ActionPlanner 和 runtime
// 始终只依赖通用 ChatClient。
func newChatClient(config llm.Config) (llm.ChatClient, error) {
	switch config.Provider {
	case "openai_compatible":
		return llm.NewOpenAICompatibleChatClient(config), nil
	case "ollama":
		if strings.TrimSpace(config.Model) == "" {
			return nil, fmt.Errorf("OLLAMA_MODEL is required")
		}
		return llm.NewOpenAICompatibleChatClient(config), nil
	case "anthropic":
		if strings.TrimSpace(config.Model) == "" {
			return nil, fmt.Errorf("ANTHROPIC_MODEL is required")
		}
		return llm.NewAnthropicChatClient(config), nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q", config.Provider)
	}
}
