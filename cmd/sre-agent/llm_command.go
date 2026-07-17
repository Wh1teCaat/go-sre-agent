package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/y2/go-sre-agent/internal/llm"
)

func pingLLM(ctx context.Context) (string, error) {
	return chatWithLLM(ctx, "这是 LLM 连通性测试。请只回复 pong。")
}

// chatWithLLM 只验证底层 ChatClient 连通性，直接返回模型原始文本。
// 诊断链路仍然通过 ActionPlanner 把模型输出解析成结构化 action。
func chatWithLLM(ctx context.Context, message string) (string, error) {
	message = strings.TrimSpace(message)
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
