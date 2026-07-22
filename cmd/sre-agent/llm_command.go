package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/y2/go-sre-agent/internal/llm"
)

// pingLLM 发送固定消息验证已配置 LLM 的最小连通性。
// 参数: ctx 控制请求取消；返回: 模型原始响应或调用错误。
func pingLLM(ctx context.Context) (string, error) {
	return chatWithLLM(ctx, "这是 LLM 连通性测试。请只回复 pong。")
}

// chatWithLLM 通过底层 ChatClient 发送文本，不进入诊断 runtime。
// 参数: ctx 控制请求取消，message 为用户文本；返回: 模型原始响应或调用错误。
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

	return client.Chat(ctx, llm.ChatRequest{
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
}
