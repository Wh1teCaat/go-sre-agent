package schema

// Memory 是历史诊断留下的只读线索。
// 它只能帮助模型形成待验证假设，不能作为本次 final evidence。
type Memory struct {
	Subject     string `json:"subject"`
	Content     string `json:"content"`
	SourceRunID string `json:"source_run_id"`
}
