package schema

import "encoding/json"

const (
	ActionTypeToolCall = "tool_call"
	ActionTypeFinal    = "final"
)

// Action 是 LLM provider 和 runtime 之间的结构化协议。
type Action struct {
	Type           string          `json:"type"`
	ThoughtSummary string          `json:"thought_summary"`
	PlanItemID     string          `json:"plan_item_id,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Args           json.RawMessage `json:"args,omitempty"`
	Final          *Diagnosis      `json:"final,omitempty"`
}
