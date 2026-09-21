package schema

import "encoding/json"

const (
	ActionTypeToolCall = "tool_call"
	// ActionTypeToolCalls 表示同一诊断步骤中的一组独立只读工具调用。
	ActionTypeToolCalls = "tool_calls"
	ActionTypeFinal     = "final"
)

// ToolCall 是批量工具 action 中的一项独立调用。每项都保留自身的计划归属、
// 工具和参数，runtime 会为其创建独立 call_id。
type ToolCall struct {
	ThoughtSummary string          `json:"thought_summary,omitempty"`
	PlanItemID     string          `json:"plan_item_id,omitempty"`
	Tool           string          `json:"tool"`
	Args           json.RawMessage `json:"args"`
}

// Action 是 LLM provider 和 runtime 之间的结构化协议。
type Action struct {
	Type           string          `json:"type"`
	ThoughtSummary string          `json:"thought_summary"`
	PlanItemID     string          `json:"plan_item_id,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
	ToolCalls      []ToolCall      `json:"tool_calls,omitempty"`
	Final          *Diagnosis      `json:"final,omitempty"`
}
