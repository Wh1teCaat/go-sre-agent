package schema

// Observation 是工具执行后给 LLM 和报告层看的事实摘要。
// Error 不为空时也会进入后续上下文，因为失败检查本身通常有诊断价值。
type Observation struct {
	Tool    string         `json:"tool"`
	Summary string         `json:"summary"`
	Error   string         `json:"error,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}
