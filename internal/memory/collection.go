package memory

import (
	"fmt"
	"strings"
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
)

func rolloutFromRun(state runstore.State) (rolloutDocument, error) {
	if !eligible(state) {
		return rolloutDocument{}, fmt.Errorf("run %q does not meet memory collection criteria", state.RunID)
	}
	service := scopeValue(state.Service)
	environment := scopeValue(state.Environment)
	status := sourceConclusionStatus(state.Diagnosis)
	document := rolloutDocument{
		Generation:             memoryGeneration,
		Kind:                   "rollout_summary",
		RunID:                  state.RunID,
		SessionID:              state.SessionID,
		Service:                service,
		Environment:            environment,
		Outcome:                string(state.Status),
		SourceConclusionStatus: status,
		ConclusionStatus:       status,
		CollectionStatus:       CollectionActive,
		SourceDigest:           stateDigest(state),
		Goal:                   normalizedText(state.Goal, 500),
		UpdatedAt:              state.UpdatedAt.UTC(),
	}
	if document.UpdatedAt.IsZero() {
		document.UpdatedAt = time.Now().UTC()
	}
	document.Phenomenon = document.Goal
	if state.Diagnosis != nil {
		if summary := normalizedText(state.Diagnosis.Summary, 500); summary != "" {
			document.Phenomenon = summary
		}
	}
	document.Title = "诊断复盘：" + document.Phenomenon
	document.Title = normalizedText(document.Title, 160)
	document.KeyObservations, document.FailureLessons, document.EvidenceSources = observationsFromRun(state)
	document.Unresolved = unresolvedFromRun(state, status)
	document.CandidateExperience = experienceFromRun(state, status, document.FailureLessons)
	document.Keywords = keywordsForRun(state, document)
	return document, nil
}

// eligible 仅收录带有实际工具观察的终态 run；这样空 skeleton、运行中状态和只有模型
// 格式错误的记录不会进入跨会话知识库。
func eligible(state runstore.State) bool {
	if !safeRunID(state.RunID) {
		return false
	}
	switch state.Status {
	case runstore.StatusCompleted, runstore.StatusFailed, runstore.StatusCancelled, runstore.StatusTimedOut:
	default:
		return false
	}
	for _, entry := range state.Trace {
		if strings.TrimSpace(entry.ToolName) != "" {
			return true
		}
	}
	return false
}

// mergeCollectionState 在 run 重试或恢复后刷新来源事实，同时保留显式生命周期和
// 纠正元数据；自动整理绝不撤销人工失效或逻辑删除操作。
func mergeCollectionState(existing, fresh rolloutDocument) rolloutDocument {
	fresh.CollectionStatus = existing.CollectionStatus
	fresh.ConclusionOverride = existing.ConclusionOverride
	fresh.CorrectionNote = existing.CorrectionNote
	fresh.InvalidationReason = existing.InvalidationReason
	if fresh.ConclusionOverride != "" {
		fresh.ConclusionStatus = fresh.ConclusionOverride
	}
	if existing.UpdatedAt.After(fresh.UpdatedAt) {
		fresh.UpdatedAt = existing.UpdatedAt
	}
	return fresh
}

// sourceConclusionStatus 保留 run 中的实际结论强度；缺少最终诊断时明确标记为
// not_recorded，绝不根据摘要推断为更强状态。
func sourceConclusionStatus(diagnosis *schema.Diagnosis) string {
	if diagnosis == nil || diagnosis.RootCause == nil {
		return "not_recorded"
	}
	status := strings.TrimSpace(diagnosis.RootCause.Status)
	if validConclusionStatus(status) {
		return status
	}
	return "not_recorded"
}

// observationsFromRun 从 trace 提取有限的观察、失败经验和原始证据定位。
func observationsFromRun(state runstore.State) ([]string, []string, []string) {
	observations := make([]string, 0, 6)
	failures := make([]string, 0, 4)
	sources := make([]string, 0, 8)
	for _, entry := range state.Trace {
		if strings.TrimSpace(entry.ToolName) == "" {
			continue
		}
		summary := normalizedText(entry.Result.Summary, 300)
		if summary == "" {
			summary = normalizedText(entry.Error, 300)
		}
		if summary == "" {
			summary = "未记录摘要"
		}
		if len(observations) < 6 {
			observations = append(observations, fmt.Sprintf("步骤 %d / %s：%s", entry.Step, entry.ToolName, summary))
		}
		callID := strings.TrimSpace(entry.CallID)
		if callID == "" {
			callID = "legacy 未记录"
		}
		sources = append(sources, fmt.Sprintf("%s / step %d / tool %s / call %s", state.RunID, entry.Step, entry.ToolName, callID))
		if strings.TrimSpace(entry.Error) != "" && len(failures) < 4 {
			failures = append(failures, fmt.Sprintf("%s 检查失败：%s", entry.ToolName, normalizedText(entry.Error, 300)))
		}
	}
	if state.Diagnosis != nil {
		for _, evidence := range state.Diagnosis.Evidence {
			sources = append(sources, fmt.Sprintf("%s / step %d / tool %s", state.RunID, evidence.Step, evidence.Tool))
		}
	}
	return uniqueLimited(observations, 6), uniqueLimited(failures, 4), uniqueLimited(sources, 10)
}

// unresolvedFromRun 记录未解决问题、失败终态和待验证事项，不把失败经验写成已识别根因。
func unresolvedFromRun(state runstore.State, conclusionStatus string) []string {
	items := make([]string, 0, 4)
	if state.Diagnosis == nil {
		items = append(items, fmt.Sprintf("运行以 %s 结束，未产生最终诊断。", state.Status))
	} else {
		for _, pending := range state.Diagnosis.PendingVerifications {
			if question := normalizedText(pending.Question, 300); question != "" {
				items = append(items, question)
			}
		}
	}
	if conclusionStatus != "identified" && conclusionStatus != "not_recorded" {
		items = append(items, "历史结论仍需使用当前环境证据重新验证。")
	}
	return uniqueLimited(items, 4)
}

// experienceFromRun 从已有结论或失败观察提取候选经验，并保留来源结论强度。
func experienceFromRun(state runstore.State, conclusionStatus string, failures []string) []string {
	items := make([]string, 0, 3)
	if state.Diagnosis != nil && state.Diagnosis.RootCause != nil {
		if statement := normalizedText(state.Diagnosis.RootCause.Statement, 400); statement != "" {
			items = append(items, fmt.Sprintf("结论强度 %s：%s", conclusionStatus, statement))
		}
	}
	if len(items) == 0 && len(failures) > 0 {
		items = append(items, "失败经验："+failures[0])
	}
	if len(items) == 0 {
		items = append(items, fmt.Sprintf("运行结果为 %s，尚未形成可复用根因结论。", state.Status))
	}
	return uniqueLimited(items, 3)
}

// keywordsForRun 只使用服务、环境、工具名、故障类型和固定故障词表做确定性索引。
func keywordsForRun(state runstore.State, document rolloutDocument) []string {
	keywords := []string{strings.ToLower(document.Service), strings.ToLower(document.Environment)}
	for _, entry := range state.Trace {
		if name := strings.ToLower(strings.TrimSpace(entry.ToolName)); name != "" {
			keywords = append(keywords, name)
		}
	}
	text := strings.ToLower(strings.Join([]string{document.Goal, document.Phenomenon, strings.Join(document.KeyObservations, " "), strings.Join(document.FailureLessons, " ")}, " "))
	for _, keyword := range []string{"redis", "postgres", "kafka", "http", "websocket", "docker", "登录", "数据库", "连接", "超时", "认证", "容器", "日志", "会话", "500"} {
		if strings.Contains(text, keyword) {
			keywords = append(keywords, keyword)
		}
	}
	if state.Diagnosis != nil && state.Diagnosis.RootCause != nil {
		if faultType := strings.ToLower(strings.TrimSpace(state.Diagnosis.RootCause.FaultType)); faultType != "" {
			keywords = append(keywords, faultType)
		}
	}
	return uniqueSorted(keywords)
}
