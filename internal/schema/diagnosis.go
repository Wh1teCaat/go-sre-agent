package schema

import "fmt"

// Evidence 是最终诊断对 trace 中某一步工具结果的引用。
// 报告层会用 step/tool 回查真实 observation，而不是盲信这里的 Summary。
type Evidence struct {
	Step    int    `json:"step"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

// PendingVerification 记录尚未完成、且会影响结论强度的验证项。它是后续检查建议，
// 不是对当前状态的断言。
type PendingVerification struct {
	Question      string         `json:"question"`
	Reason        string         `json:"reason,omitempty"`
	Target        TargetIdentity `json:"target,omitempty"`
	SuggestedTool string         `json:"suggested_tool,omitempty"`
}

// RootCause 是最终诊断对根因的显式声明。结论强度由结构化 status 表达：
// identified 必须引用真实 trace 证据，证据不足时用 suspected/undetermined，
// 而不是在自由文本里下断言。
type RootCause struct {
	Status    string     `json:"status"` // identified | suspected | undetermined
	FaultType string     `json:"fault_type,omitempty"`
	Statement string     `json:"statement,omitempty"`
	Evidence  []Evidence `json:"evidence,omitempty"`
}

// Diagnosis 是 final action 的业务内容。Evidence 是所有证据引用的兼容字段；
// 新输出还要用 SupportingEvidence、CounterEvidence 和 PendingVerifications 明确
// 区分支撑、反证与待验证事项。
type Diagnosis struct {
	Summary              string                `json:"summary"`
	RootCause            *RootCause            `json:"root_cause,omitempty"`
	Evidence             []Evidence            `json:"evidence,omitempty"`
	SupportingEvidence   []Evidence            `json:"supporting_evidence,omitempty"`
	CounterEvidence      []Evidence            `json:"counter_evidence,omitempty"`
	PendingVerifications []PendingVerification `json:"pending_verifications,omitempty"`
	Coverage             []CoverageItem        `json:"coverage,omitempty"`
	Recommendations      []string              `json:"recommendations,omitempty"`
}

// EvidenceKey 用 step 和 tool 构造 evidence 与 trace 的匹配键。
func EvidenceKey(step int, tool string) string {
	return fmt.Sprintf("%d:%s", step, tool)
}
