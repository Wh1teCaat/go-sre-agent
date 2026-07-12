package schema

import "encoding/json"

const (
	ActionTypePlan     = "plan"
	ActionTypeToolCall = "tool_call"
	ActionTypeFinal    = "final"
)

// Action 是 LLM provider 和 runtime 之间的结构化协议。
// plan 表示更新检查清单，tool_call 表示执行一个只读工具，final 表示结束诊断并给出结论。
type Action struct {
	Type           string          `json:"type"`
	ThoughtSummary string          `json:"thought_summary"`
	Plan           *Plan           `json:"plan,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
	Final          *Diagnosis      `json:"final,omitempty"`
}

func (a Action) IsFinal() bool {
	return a.Type == ActionTypeFinal
}
