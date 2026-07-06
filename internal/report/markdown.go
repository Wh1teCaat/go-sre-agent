package report

import (
	"fmt"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
)

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

	if len(input.Trace) > 0 {
		b.WriteString("## Trace\n\n")
		for _, entry := range input.Trace {
			status := "ok"
			if entry.Error != "" {
				status = entry.Error
			}
			b.WriteString(fmt.Sprintf("- Step %d `%s` (%s): %s\n", entry.Step, entry.ToolName, entry.Duration, status))
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

	return b.String()
}

// traceBackedEvidence 只保留能在 trace 中找到同 step/tool 的证据。
// 展示摘要优先使用工具真实 observation，模型 evidence 只负责指向哪一步。
func traceBackedEvidence(input Input) ([]schema.Evidence, []schema.Evidence) {
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

	verified := make([]schema.Evidence, 0, len(input.Diagnosis.Evidence))
	filtered := make([]schema.Evidence, 0)
	for _, evidence := range input.Diagnosis.Evidence {
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
