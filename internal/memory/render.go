package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/tools"
	"gopkg.in/yaml.v3"
)

func renderRollout(document rolloutDocument) ([]byte, error) {
	body := renderRolloutBody(document)
	document.Generation = memoryGeneration
	document.Kind = "rollout_summary"
	document.ContentDigest = ""
	digest, err := digestDocument(document, body)
	if err != nil {
		return nil, err
	}
	document.ContentDigest = digest
	return encodeDocument(document, body)
}

// renderRaw 生成候选经验汇总，不复制原始工具日志。
func renderRaw(document rawDocument) ([]byte, error) {
	body := renderRawBody(document)
	document.Generation = memoryGeneration
	document.Kind = "raw_memories"
	document.ContentDigest = ""
	digest, err := digestDocument(document, body)
	if err != nil {
		return nil, err
	}
	document.ContentDigest = digest
	return encodeDocument(document, body)
}

// renderIndex 生成按主题分组的知识索引。
func renderIndex(document indexDocument) ([]byte, error) {
	body := renderIndexBody(document)
	document.Generation = memoryGeneration
	document.Kind = "memory_index"
	document.ContentDigest = ""
	digest, err := digestDocument(document, body)
	if err != nil {
		return nil, err
	}
	document.ContentDigest = digest
	return encodeDocument(document, body)
}

// renderSummary 生成不重复复盘内容的轻量导航。
func renderSummary(document summaryDocument) ([]byte, error) {
	body := renderSummaryBody(document)
	document.Generation = memoryGeneration
	document.Kind = "memory_summary"
	document.ContentDigest = ""
	digest, err := digestDocument(document, body)
	if err != nil {
		return nil, err
	}
	document.ContentDigest = digest
	return encodeDocument(document, body)
}

// verifyRollout 验证 metadata、正文和摘要是否仍与生成时一致。
func verifyRollout(document rolloutDocument, body string) error {
	if document.Generation != memoryGeneration || document.Kind != "rollout_summary" {
		return fmt.Errorf("file is not a generated rollout summary")
	}
	if !validCollectionStatus(document.CollectionStatus) || !validConclusionStatus(document.SourceConclusionStatus) || !validConclusionStatus(document.ConclusionStatus) {
		return fmt.Errorf("invalid rollout lifecycle metadata")
	}
	if document.ConclusionOverride != "" && !validConclusionStatus(document.ConclusionOverride) {
		return fmt.Errorf("invalid rollout conclusion override")
	}
	return verifyDigest(document, body, document.ContentDigest)
}

// verifyRaw 验证候选经验汇总未被手工修改。
func verifyRaw(document rawDocument, body string) error {
	if document.Generation != memoryGeneration || document.Kind != "raw_memories" {
		return fmt.Errorf("file is not generated raw memories")
	}
	return verifyDigest(document, body, document.ContentDigest)
}

// verifyIndex 验证主题索引未被手工修改。
func verifyIndex(document indexDocument, body string) error {
	if document.Generation != memoryGeneration || document.Kind != "memory_index" {
		return fmt.Errorf("file is not generated memory index")
	}
	return verifyDigest(document, body, document.ContentDigest)
}

// verifySummary 验证轻量导航未被手工修改。
func verifySummary(document summaryDocument, body string) error {
	if document.Generation != memoryGeneration || document.Kind != "memory_summary" {
		return fmt.Errorf("file is not generated memory summary")
	}
	return verifyDigest(document, body, document.ContentDigest)
}

// verifyDigest 以清空 content_digest 后的标准 YAML 与原始正文计算摘要。
func verifyDigest(document any, body, actual string) error {
	if strings.TrimSpace(actual) == "" {
		return fmt.Errorf("generated content digest is missing")
	}
	switch typed := document.(type) {
	case rolloutDocument:
		typed.ContentDigest = ""
		document = typed
	case rawDocument:
		typed.ContentDigest = ""
		document = typed
	case indexDocument:
		typed.ContentDigest = ""
		document = typed
	case summaryDocument:
		typed.ContentDigest = ""
		document = typed
	default:
		return fmt.Errorf("unsupported generated document")
	}
	expected, err := digestDocument(document, body)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("generated content digest does not match")
	}
	return nil
}

// digestDocument 对前置 metadata 和正文统一计算摘要。
func digestDocument(document any, body string) (string, error) {
	header, err := yaml.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode memory metadata: %w", err)
	}
	sum := sha256.Sum256([]byte("---\n" + string(header) + "---\n" + body))
	return hex.EncodeToString(sum[:]), nil
}

// encodeDocument 组合 YAML front matter 和 Markdown 正文。
func encodeDocument(document any, body string) ([]byte, error) {
	header, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode memory metadata: %w", err)
	}
	return []byte("---\n" + string(header) + "---\n" + body), nil
}

// splitDocument 分离由本包生成的 YAML front matter 与 Markdown 正文。
func splitDocument(data []byte) ([]byte, string, error) {
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", fmt.Errorf("generated front matter is missing")
	}
	rest := text[len("---\n"):]
	index := strings.Index(rest, "---\n")
	if index < 0 {
		return nil, "", fmt.Errorf("generated front matter terminator is missing")
	}
	return []byte(rest[:index]), rest[index+len("---\n"):], nil
}

// renderRolloutBody 构造单次复盘的人类可读正文。
func renderRolloutBody(document rolloutDocument) string {
	var content strings.Builder
	fmt.Fprintf(&content, "# %s\n\n", markdownText(document.Title))
	fmt.Fprintf(&content, "- run_id: `%s`\n- session_id: `%s`\n- service: `%s`\n- environment: `%s`\n- outcome: `%s`\n- conclusion_status: `%s`\n", document.RunID, fallbackID(document.SessionID), document.Service, document.Environment, document.Outcome, document.ConclusionStatus)
	if document.CollectionStatus != CollectionActive {
		fmt.Fprintf(&content, "- collection_status: `%s`\n", document.CollectionStatus)
	}
	content.WriteString("\n## 故障现象与排查目标\n\n")
	content.WriteString("- " + markdownText(document.Phenomenon) + "\n")
	content.WriteString("- 目标：" + markdownText(document.Goal) + "\n")
	content.WriteString("\n## 关键检查与观察\n\n")
	writeBulletSection(&content, document.KeyObservations, "未记录可收录的工具观察。")
	content.WriteString("\n## 最终结论及强度\n\n")
	content.WriteString("- 强度：`" + document.ConclusionStatus + "`（来源为 `" + document.SourceConclusionStatus + "`）。\n")
	if document.CorrectionNote != "" {
		content.WriteString("- 修正：" + markdownText(document.CorrectionNote) + "\n")
	}
	if document.CollectionStatus == CollectionInvalidated || document.CollectionStatus == CollectionDeleted {
		content.WriteString("- 状态说明：" + markdownText(document.InvalidationReason) + "\n")
	}
	content.WriteString("\n## 未解决问题\n\n")
	writeBulletSection(&content, document.Unresolved, "没有记录额外待验证项；历史结果仍需验证当前状态。")
	content.WriteString("\n## 有效步骤和失败经验\n\n")
	writeBulletSection(&content, document.CandidateExperience, "未提取到额外候选经验。")
	writeBulletSection(&content, document.FailureLessons, "未记录工具失败经验。")
	content.WriteString("\n## 证据来源\n\n")
	writeBulletSection(&content, document.EvidenceSources, "未记录 trace 来源。")
	content.WriteString("\n## 适用限制\n\n")
	content.WriteString("- 历史结果不能代表当前状态；后续诊断必须重新检查当前配置、目标身份和证据。\n")
	return content.String()
}

// renderRawBody 构造候选经验汇总正文。
func renderRawBody(document rawDocument) string {
	var content strings.Builder
	content.WriteString("# 待整理经验汇总\n\n")
	content.WriteString("本文件由 rollout summaries 确定性生成，不是原始工具日志，也不会整体注入模型。\n")
	for _, entry := range document.Entries {
		fmt.Fprintf(&content, "\n## %s\n\n", entry.RunID)
		fmt.Fprintf(&content, "- 服务与环境：%s / %s\n- 故障关键词：%s\n- 结论强度：`%s`\n- 收录状态：`%s`\n- 适用条件：%s\n- 来源复盘：`rollout_summaries/%s.md`\n", markdownText(entry.Service), markdownText(entry.Environment), markdownText(strings.Join(entry.Keywords, "、")), entry.ConclusionStatus, entry.CollectionStatus, markdownText(entry.Applicability), entry.RunID)
		content.WriteString("- 候选经验：\n")
		writeBulletSection(&content, entry.Experience, "未提取。")
		content.WriteString("- 失败教训：\n")
		writeBulletSection(&content, entry.FailureLessons, "未记录。")
	}
	if len(document.Entries) == 0 {
		content.WriteString("\n- 当前没有已收录的复盘。\n")
	}
	return content.String()
}

// renderIndexBody 构造按主题的知识索引正文。
func renderIndexBody(document indexDocument) string {
	var content strings.Builder
	content.WriteString("# 跨会话知识索引\n\n")
	content.WriteString("索引只帮助提出历史假设；最终结论必须由本次 run 的 trace 证据支撑。\n")
	for _, item := range document.Topics {
		fmt.Fprintf(&content, "\n## 主题：%s\n\n", markdownText(item.Subject))
		fmt.Fprintf(&content, "- 适用范围：%s，%s。\n- 检索关键词：%s。\n- 可复用知识：\n", markdownText(item.Service), markdownText(item.Environment), markdownText(strings.Join(item.Keywords, "、")))
		writeBulletSection(&content, item.Knowledge, "未提取到额外候选经验。")
		content.WriteString("- 来源：\n")
		for _, runID := range item.RunIDs {
			fmt.Fprintf(&content, "  - rollout_summaries/%s.md\n", runID)
		}
		content.WriteString("- 使用限制：新诊断必须检查当前配置、目标身份和本次证据。\n")
	}
	if len(document.Topics) == 0 {
		content.WriteString("\n- 当前没有可检索主题。\n")
	}
	return content.String()
}

// renderSummaryBody 构造不复制复盘正文的轻量导航。
func renderSummaryBody(document summaryDocument) string {
	var content strings.Builder
	content.WriteString("# 跨会话记忆导航\n\n")
	content.WriteString("按服务、环境和关键词先定位主题，再读取 MEMORY.md 与对应 rollout summary。\n")
	for _, item := range document.Topics {
		fmt.Fprintf(&content, "- `%s`：适用于 %s / %s；关键词：%s。\n", markdownText(item.Subject), markdownText(item.Service), markdownText(item.Environment), markdownText(strings.Join(item.Keywords, "、")))
	}
	if len(document.Topics) == 0 {
		content.WriteString("- 当前没有知识主题。\n")
	}
	return content.String()
}

// renderHint 将一份来源复盘压缩为模型可见的历史材料，并保留结论强度和来源。
func renderHint(document rolloutDocument) string {
	var content strings.Builder
	fmt.Fprintf(&content, "历史复盘 `%s`（服务 %s，环境 %s，运行结果 %s，结论强度 %s）。\n", document.RunID, document.Service, document.Environment, document.Outcome, document.ConclusionStatus)
	content.WriteString("故障现象：" + markdownText(document.Phenomenon) + "\n")
	content.WriteString("关键观察：\n")
	writeBulletSection(&content, document.KeyObservations, "未记录。")
	content.WriteString("候选经验：\n")
	writeBulletSection(&content, document.CandidateExperience, "未记录。")
	content.WriteString("来源：rollout_summaries/" + document.RunID + ".md；必要时回查 .runs/" + document.RunID + ".json。\n")
	content.WriteString("限制：历史资料只能辅助提出假设，不能作为当前结论证据。\n")
	return content.String()
}

// writeBulletSection 输出已脱敏、单行化的 Markdown 列表。
func writeBulletSection(content *strings.Builder, values []string, fallback string) {
	if len(values) == 0 {
		content.WriteString("- " + markdownText(fallback) + "\n")
		return
	}
	for _, value := range values {
		content.WriteString("- " + markdownText(value) + "\n")
	}
}

// normalizedText 在进入 memory 文件前再次脱敏并限制长度。
func normalizedText(value string, limit int) string {
	value = strings.Join(strings.Fields(tools.RedactSensitive(value)), " ")
	return truncateBytes(value, limit)
}

// markdownText 防止持久化文本意外闭合 Markdown 代码片段或扩展为多行结构。
func markdownText(value string) string {
	value = normalizedText(value, 600)
	if value == "" {
		return "未记录"
	}
	return strings.ReplaceAll(value, "`", "'")
}

// scopeValue 为缺失的服务或环境标签提供保守的 unknown，而不从地址推测范围。
func scopeValue(value string) string {
	value = normalizedText(value, 120)
	if value == "" {
		return "unknown"
	}
	return value
}

// stateDigest 对原始 run 快照计算来源摘要，只用于判断来源版本而不替代 run JSON。
func stateDigest(state runstore.State) string {
	encoded, err := json.Marshal(state)
	if err != nil {
		return "unavailable"
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// uniqueLimited 去除重复文本并保留出现顺序。
func uniqueLimited(values []string, limit int) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizedText(value, 600)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

// uniqueSorted 生成稳定、去重的关键词或来源标识列表。
func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// truncateBytes 在 UTF-8 边界裁剪文本，保证模型与生成文件预算按字节生效。
func truncateBytes(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	const suffix = "..."
	if limit <= len(suffix) {
		return suffix[:limit]
	}
	end := limit - len(suffix)
	for end > 0 && (value[end]&0xc0) == 0x80 {
		end--
	}
	return value[:end] + suffix
}

// fallbackID 让旧 run 缺失 session_id 时明确显示 legacy，而不是伪造关联关系。
func fallbackID(value string) string {
	if strings.TrimSpace(value) == "" {
		return "legacy 未记录"
	}
	return value
}

// validConclusionStatus 只接受 runtime 已使用的结论强度，加上旧 run 缺失字段标识。
func validConclusionStatus(status string) bool {
	switch status {
	case "identified", "suspected", "undetermined", "not_recorded":
		return true
	default:
		return false
	}
}

// conclusionStrength 返回可安全比较的结论等级；not_recorded 永远不能被自动升级。
func conclusionStrength(status string) int {
	switch status {
	case "identified":
		return 3
	case "suspected":
		return 2
	case "undetermined":
		return 1
	default:
		return 0
	}
}

// validCollectionStatus 校验收录生命周期状态。
func validCollectionStatus(status CollectionStatus) bool {
	return status == CollectionActive || status == CollectionInvalidated || status == CollectionDeleted
}

// safeRunID 拒绝路径分隔符和非文件名安全字符。
func safeRunID(runID string) bool {
	if strings.TrimSpace(runID) == "" || runID != filepath.Base(runID) {
		return false
	}
	for _, character := range runID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
