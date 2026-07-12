package schema

// Plan 是 LLM 为当前诊断维护的检查清单。
// 它只是覆盖面约束，不是固定工作流；后续 plan action 可以追加或更新条目。
type Plan struct {
	Reason string     `json:"reason,omitempty"`
	Items  []PlanItem `json:"items"`
}

// PlanItem 表示一个需要被工具观察或明确标记为证据不足的检查项。
type PlanItem struct {
	ID     string `json:"id"`
	Goal   string `json:"goal"`
	Status string `json:"status,omitempty"` // pending | done | blocked
	Reason string `json:"reason,omitempty"`
}

// CoverageItem 是 final diagnosis 对 plan item 的覆盖声明。
// Evidence 仍必须引用真实 trace，不能只靠模型自述。
type CoverageItem struct {
	PlanItemID string     `json:"plan_item_id"`
	Status     string     `json:"status"` // done | blocked | insufficient
	Evidence   []Evidence `json:"evidence,omitempty"`
	Note       string     `json:"note,omitempty"`
}
