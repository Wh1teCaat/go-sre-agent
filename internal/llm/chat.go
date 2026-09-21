package llm

import "context"

// Role 是与 provider 无关的角色定义，具体 ChatClient 再映射到上游模型
// API 需要的角色名称。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type OutputMode string

const (
	OutputText OutputMode = "text"
	OutputJSON OutputMode = "json"
)

type Message struct {
	Role    Role
	Content string
}

// ChatRequest 是项目内部使用的通用模型请求。
// 它只表达项目需要的通用能力，厂商特有字段由具体 ChatClient 适配。
type ChatRequest struct {
	Model       string
	Messages    []Message
	OutputMode  OutputMode
	Temperature float64
}

// ChatClient 把通用 chat 请求适配到某个具体模型 provider，比如
// OpenAI-compatible API、Ollama 或 Anthropic。
type ChatClient interface {
	Chat(ctx context.Context, request ChatRequest) (string, error)
}
