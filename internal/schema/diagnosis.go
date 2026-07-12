package schema

import "fmt"

// Evidence 是最终诊断对 trace 中某一步工具结果的引用。
// 报告层会用 step/tool 回查真实 observation，而不是盲信这里的 Summary。
type Evidence struct {
	Step    int    `json:"step"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

// Diagnosis 是 final action 的业务内容：结论、证据引用和后续建议。
type Diagnosis struct {
	Summary         string         `json:"summary"`
	Evidence        []Evidence     `json:"evidence,omitempty"`
	Coverage        []CoverageItem `json:"coverage,omitempty"`
	Recommendations []string       `json:"recommendations,omitempty"`
}

// EvidenceKey 用 step 和 tool 构造 evidence 与 trace 的匹配键。
func EvidenceKey(step int, tool string) string {
	return fmt.Sprintf("%d:%s", step, tool)
}
