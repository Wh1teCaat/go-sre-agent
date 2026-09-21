package agent

import (
	"encoding/json"
	"sort"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

const (
	minPromptContextBytes     = 1024
	minPromptObservationBytes = 512
)

// observationsForPrompt 从完整 trace 构造受预算约束的 LLM 上下文副本。完整工具
// 输出仍只保存在 trace 与 run JSON；该函数绝不修改它们。
func observationsForPrompt(entries []trace.Entry, contextBudgetBytes, toolOutputBudgetBytes int) []schema.Observation {
	if contextBudgetBytes <= 0 {
		contextBudgetBytes = defaultContextBudgetBytes
	}
	if contextBudgetBytes < minPromptContextBytes {
		contextBudgetBytes = minPromptContextBytes
	}
	if toolOutputBudgetBytes < minPromptObservationBytes {
		toolOutputBudgetBytes = minPromptObservationBytes
	}

	selected := make([]schema.Observation, 0, len(entries))
	used := 0
	// 从最新观测开始取样，优先保留当前诊断最相关的证据；最终再恢复时间顺序。
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if !hasTraceObservation(entry.Result) {
			continue
		}
		observation := promptObservation(entry, toolOutputBudgetBytes)
		size := serializedSize(observation)
		if len(selected) > 0 && used+size > contextBudgetBytes {
			continue
		}
		if size > contextBudgetBytes {
			observation = minimalPromptObservation(entry, serializedSize(entry.Result), contextBudgetBytes)
			size = serializedSize(observation)
		}
		if size > contextBudgetBytes && len(selected) > 0 {
			continue
		}
		selected = append(selected, observation)
		used += size
	}
	for left, right := 0, len(selected)-1; left < right; left, right = left+1, right-1 {
		selected[left], selected[right] = selected[right], selected[left]
	}
	return selected
}

// promptObservation 优先保留完整、小型观测；超过单条工具输出预算时，只保留
// 结构化状态、有限事实和可回查的 trace_reference。
func promptObservation(entry trace.Entry, budget int) schema.Observation {
	observation := entry.Result
	observation.Step = entry.Step
	observation.PlanItemID = entry.PlanItemID
	if serializedSize(observation) <= budget {
		return observation
	}
	return minimalPromptObservation(entry, serializedSize(observation), budget)
}

// minimalPromptObservation 构造可在指定预算内传给模型的最小观测，并保留可回查
// 的 trace 引用；调用方仍可通过原始 run 记录取得完整输出。
func minimalPromptObservation(entry trace.Entry, originalBytes, budget int) schema.Observation {
	result := entry.Result
	observation := schema.Observation{
		Step:             entry.Step,
		PlanItemID:       truncateText(entry.PlanItemID, 120),
		Tool:             truncateText(result.Tool, 120),
		Summary:          truncateText(result.Summary, 240),
		Error:            truncateText(result.Error, 240),
		CheckStatus:      truncateText(result.CheckStatus, 80),
		TargetHealth:     truncateText(result.TargetHealth, 80),
		Target:           truncateTarget(result.Target),
		ObservedAt:       result.ObservedAt,
		Facts:            compactFacts(result.Facts),
		Data:             map[string]any{"original_bytes": originalBytes},
		ContextTruncated: true,
		TraceReference: &schema.TraceReference{
			Step:   entry.Step,
			Tool:   truncateText(entry.ToolName, 120),
			CallID: truncateText(entry.CallID, 120),
		},
	}
	if serializedSize(observation) <= budget {
		return observation
	}
	observation.Facts = nil
	observation.Summary = truncateText(result.Summary, 120)
	observation.Error = truncateText(result.Error, 120)
	observation.Target.ID = truncateText(result.Target.ID, 120)
	if serializedSize(observation) <= budget {
		return observation
	}
	// 在极小预算下仍保留步骤、工具和调用标识，删除可从 trace 回查的其余大字段。
	observation.Summary = truncateText(result.Summary, 48)
	observation.Error = truncateText(result.Error, 48)
	observation.Target = schema.TargetIdentity{}
	return observation
}

// hasTraceObservation 判断 trace 条目是否包含可作为模型上下文的工具观测。
func hasTraceObservation(observation schema.Observation) bool {
	return observation.Tool != "" || observation.Summary != "" || observation.Error != "" || len(observation.Data) > 0 || len(observation.Facts) > 0
}

// compactFacts 稳定选取有限的标量事实，避免嵌套或大对象突破提示上下文预算。
func compactFacts(facts []schema.Fact) []schema.Fact {
	if len(facts) == 0 {
		return nil
	}
	const maxFacts = 4
	limited := append([]schema.Fact(nil), facts...)
	sort.SliceStable(limited, func(left, right int) bool {
		return limited[left].Key < limited[right].Key
	})
	if len(limited) > maxFacts {
		limited = limited[:maxFacts]
	}
	for index := range limited {
		limited[index].Key = truncateText(limited[index].Key, 80)
		switch value := limited[index].Value.(type) {
		case string:
			limited[index].Value = truncateText(value, 160)
		case nil, bool, int, int64, float64, json.Number:
		default:
			limited[index].Value = "[已省略]"
		}
		limited[index].Target = truncateTarget(limited[index].Target)
	}
	return limited
}

// truncateTarget 裁剪目标身份字段，同时不改变调用方持有的原始目标对象。
func truncateTarget(target schema.TargetIdentity) schema.TargetIdentity {
	target.Kind = truncateText(target.Kind, 80)
	target.ID = truncateText(target.ID, 240)
	return target
}

// truncateText 按 UTF-8 字节上限裁剪文本，并以省略号标识内容不完整。
func truncateText(value string, limit int) string {
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

// serializedSize 返回值在 JSON 请求中的近似字节数；序列化失败时保守返回零。
func serializedSize(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(encoded)
}
