package report

import (
	"fmt"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type Input struct {
	Goal      string
	Diagnosis schema.Diagnosis
	Plan      schema.Plan
	Trace     []trace.Entry
}

// Markdown 根据最终诊断和内部 trace 生成 Markdown 报告。
// 报告里的证据会重新回查 trace，避免直接采信模型声明的不存在证据。
func Markdown(input Input) string {
	var b strings.Builder
	b.WriteString("# SRE Diagnosis Report\n\n")
	if input.Goal != "" {
		b.WriteString("## Goal\n\n")
		b.WriteString(input.Goal)
		b.WriteString("\n\n")
	}

	b.WriteString("## Summary\n\n")
	if input.Diagnosis.Summary == "" {
		b.WriteString("No final diagnosis was produced.\n\n")
	} else {
		b.WriteString(input.Diagnosis.Summary)
		b.WriteString("\n\n")
	}

	if rootCause := input.Diagnosis.RootCause; rootCause != nil {
		// 根因状态放在证据之前：读者先看到结论强度，再核对支撑证据。
		b.WriteString("## Root Cause\n\n")
		b.WriteString(fmt.Sprintf("- Status: `%s`\n", rootCause.Status))
		if rootCause.FaultType != "" {
			b.WriteString(fmt.Sprintf("- Fault type: `%s`\n", rootCause.FaultType))
		}
		if rootCause.Statement != "" {
			b.WriteString("- Statement: " + rootCause.Statement + "\n")
		}
		for _, evidence := range rootCause.Evidence {
			b.WriteString(fmt.Sprintf("- Evidence: Step %d `%s`\n", evidence.Step, evidence.Tool))
		}
		b.WriteString("\n")
	}

	verifiedEvidence, filteredEvidence := traceBackedEvidence(input)
	if len(verifiedEvidence) > 0 {
		b.WriteString("## Evidence\n\n")
		for _, evidence := range verifiedEvidence {
			b.WriteString(fmt.Sprintf("- Step %d `%s`: %s\n", evidence.Step, evidence.Tool, evidence.Summary))
		}
		b.WriteString("\n")
	}

	if len(filteredEvidence) > 0 {
		b.WriteString("## Filtered Evidence Claims\n\n")
		for _, evidence := range filteredEvidence {
			b.WriteString(fmt.Sprintf("- Step %d `%s`: no matching trace entry\n", evidence.Step, evidence.Tool))
		}
		b.WriteString("\n")
	}

	if len(input.Diagnosis.SupportingEvidence) > 0 {
		b.WriteString("## Supporting Evidence\n\n")
		writeTraceBackedEvidence(&b, input, input.Diagnosis.SupportingEvidence)
		b.WriteString("\n")
	}

	if len(input.Diagnosis.CounterEvidence) > 0 {
		b.WriteString("## Counter Evidence\n\n")
		writeTraceBackedEvidence(&b, input, input.Diagnosis.CounterEvidence)
		b.WriteString("\n")
	}

	if len(input.Diagnosis.PendingVerifications) > 0 {
		b.WriteString("## Pending Verifications\n\n")
		for _, item := range input.Diagnosis.PendingVerifications {
			b.WriteString("- ")
			b.WriteString(item.Question)
			if item.Reason != "" {
				b.WriteString(": ")
				b.WriteString(item.Reason)
			}
			if item.Target.Kind != "" && item.Target.ID != "" {
				b.WriteString(fmt.Sprintf(" [target=%s:%s]", item.Target.Kind, item.Target.ID))
			}
			if item.SuggestedTool != "" {
				b.WriteString(fmt.Sprintf(" [tool=%s]", item.SuggestedTool))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if facts := traceBackedFacts(input); len(facts) > 0 {
		b.WriteString("## Structured Facts\n\n")
		for _, fact := range facts {
			observedAt := "unknown time"
			if !fact.ObservedAt.IsZero() {
				observedAt = fact.ObservedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			target := "unknown target"
			if fact.Target.Kind != "" && fact.Target.ID != "" {
				target = fact.Target.Kind + ":" + fact.Target.ID
			}
			b.WriteString(fmt.Sprintf("- `%s` = `%v` [target=%s, observed_at=%s]\n", fact.Key, fact.Value, target, observedAt))
		}
		b.WriteString("\n")
	}

	if len(input.Diagnosis.Coverage) > 0 {
		b.WriteString("## Plan Coverage\n\n")
		for _, item := range input.Diagnosis.Coverage {
			label := planItemGoal(input.Plan, item.PlanItemID)
			if label == "" {
				label = item.PlanItemID
			}
			b.WriteString(fmt.Sprintf("- `%s` %s: %s", item.Status, item.PlanItemID, label))
			if item.Note != "" {
				b.WriteString(" - ")
				b.WriteString(item.Note)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(input.Trace) > 0 {
		b.WriteString("## Trace\n\n")
		for _, entry := range input.Trace {
			status := "ok"
			if entry.Error != "" {
				status = entry.Error
			}
			label := entry.ToolName
			if label == "" {
				label = entry.ActionType
			}
			b.WriteString(fmt.Sprintf("- Step %d `%s` (%s): %s", entry.Step, label, entry.Duration, status))
			if entry.Model != "" || entry.LLMAttempts > 0 {
				b.WriteString(fmt.Sprintf(" [model=%s, llm=%s, attempts=%d]", entry.Model, entry.LLMDuration, entry.LLMAttempts))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(input.Diagnosis.Recommendations) > 0 {
		b.WriteString("## Recommendations\n\n")
		for _, recommendation := range input.Diagnosis.Recommendations {
			b.WriteString("- ")
			b.WriteString(recommendation)
			b.WriteString("\n")
		}
	}

	return tools.RedactSensitive(b.String())
}

func planItemGoal(plan schema.Plan, id string) string {
	for _, item := range plan.Items {
		if item.ID == id {
			return item.Goal
		}
	}
	return ""
}

// traceBackedEvidence 只保留能在 trace 中找到同 step/tool 的证据。
// 展示摘要优先使用工具真实 observation，模型 evidence 只负责指向哪一步。
func traceBackedEvidence(input Input) ([]schema.Evidence, []schema.Evidence) {
	return traceBackedEvidenceFor(input, input.Diagnosis.Evidence)
}

// traceBackedEvidenceFor 只保留能在 trace 中找到同 step/tool 的证据，展示摘要
// 始终以工具真实 observation 为准，模型证据只负责指向对应步骤。
func traceBackedEvidenceFor(input Input, claims []schema.Evidence) ([]schema.Evidence, []schema.Evidence) {
	traceSummaries := map[string]string{}
	for _, entry := range input.Trace {
		summary := entry.Result.Summary
		if summary == "" {
			summary = entry.Result.Error
		}
		if summary == "" {
			summary = entry.Error
		}
		if summary == "" {
			continue
		}
		traceSummaries[schema.EvidenceKey(entry.Step, entry.ToolName)] = summary
	}

	verified := make([]schema.Evidence, 0, len(claims))
	filtered := make([]schema.Evidence, 0)
	for _, evidence := range claims {
		summary, ok := traceSummaries[schema.EvidenceKey(evidence.Step, evidence.Tool)]
		if !ok {
			filtered = append(filtered, evidence)
			continue
		}
		verified = append(verified, schema.Evidence{
			Step:    evidence.Step,
			Tool:    evidence.Tool,
			Summary: summary,
		})
	}
	return verified, filtered
}

// writeTraceBackedEvidence 以 trace 回填的摘要写出某一种证据角色。
func writeTraceBackedEvidence(b *strings.Builder, input Input, claims []schema.Evidence) {
	verified, filtered := traceBackedEvidenceFor(input, claims)
	for _, evidence := range verified {
		b.WriteString(fmt.Sprintf("- Step %d `%s`: %s\n", evidence.Step, evidence.Tool, evidence.Summary))
	}
	for _, evidence := range filtered {
		b.WriteString(fmt.Sprintf("- Step %d `%s`: no matching trace entry\n", evidence.Step, evidence.Tool))
	}
}

// traceBackedFacts 只展示被 final.evidence 引用的 observation 事实，防止未被
// 最终结论采用的辅助检查在报告中被误读为根因依据。
func traceBackedFacts(input Input) []schema.Fact {
	wanted := make(map[string]struct{}, len(input.Diagnosis.Evidence))
	for _, evidence := range input.Diagnosis.Evidence {
		wanted[schema.EvidenceKey(evidence.Step, evidence.Tool)] = struct{}{}
	}
	facts := []schema.Fact{}
	for _, entry := range input.Trace {
		if _, ok := wanted[schema.EvidenceKey(entry.Step, entry.ToolName)]; !ok {
			continue
		}
		facts = append(facts, entry.Result.Facts...)
	}
	return facts
}
