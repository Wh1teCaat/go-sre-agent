package schema

import "encoding/json"

const (
	ActionTypeToolCall = "tool_call"
	ActionTypeFinal    = "final"
)

// Action 是 LLM provider 和 runtime 之间的结构化协议。
// tool_call 表示执行一个只读工具，final 表示结束诊断并给出结论。
type Action struct {
	Type           string          `json:"type"`
	ThoughtSummary string          `json:"thought_summary"`
	Tool           string          `json:"tool,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
	Final          *Diagnosis      `json:"final,omitempty"`
}

func (a Action) IsFinal() bool {
	return a.Type == ActionTypeFinal
}
