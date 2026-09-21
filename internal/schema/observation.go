package schema

import "time"

const (
	// CheckExecutionCompleted 表示检查动作已返回可供诊断的结果；目标仍可能不健康。
	CheckExecutionCompleted = "completed"
	// CheckExecutionFailed 表示检查没有取得可用结果，不能把它当作目标状态。
	CheckExecutionFailed = "failed"
	// CheckExecutionCancelled 表示检查在任务取消前未完成。
	CheckExecutionCancelled = "cancelled"
	// CheckExecutionTimedOut 表示检查在时限内没有完成。
	CheckExecutionTimedOut = "timed_out"
)

const (
	// TargetHealthHealthy 表示该次检查在明确范围内观察到目标健康。
	TargetHealthHealthy = "healthy"
	// TargetHealthUnhealthy 表示该次检查在明确范围内观察到目标不健康。
	TargetHealthUnhealthy = "unhealthy"
	// TargetHealthDegraded 表示目标可用但存在明确退化信号。
	TargetHealthDegraded = "degraded"
	// TargetHealthUnknown 表示检查结果不足以判断目标健康状态。
	TargetHealthUnknown = "unknown"
	// TargetHealthNotApplicable 表示该工具只收集辅助信息，不直接评估目标健康。
	TargetHealthNotApplicable = "not_applicable"
)

// TargetIdentity 标识某一条观测实际作用的对象。ID 必须是可安全持久化的标识，
// 不得包含密码、令牌、完整 DSN 等敏感参数。
type TargetIdentity struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Fact 是带时间、目标和键名的原子事实。Value 保留工具已脱敏后的原始 JSON 值，
// 让报告和后续校验无需从自由文本 summary 反向推断。
type Fact struct {
	Key        string         `json:"key"`
	Value      any            `json:"value"`
	ObservedAt time.Time      `json:"observed_at"`
	Target     TargetIdentity `json:"target"`
}

// Observation 是工具执行后给 LLM 和报告层看的事实摘要。
// CheckStatus 与 TargetHealth 分开：前者描述检查是否拿到可用结果，后者只描述
// 被检查目标在本次、该范围内的状态。Error 不为空时也会进入后续上下文。
type Observation struct {
	Step         int            `json:"step,omitempty"`
	PlanItemID   string         `json:"plan_item_id,omitempty"`
	Tool         string         `json:"tool"`
	Summary      string         `json:"summary"`
	Error        string         `json:"error,omitempty"`
	CheckStatus  string         `json:"check_status,omitempty"`
	TargetHealth string         `json:"target_health,omitempty"`
	Target       TargetIdentity `json:"target,omitempty"`
	ObservedAt   time.Time      `json:"observed_at,omitempty"`
	Facts        []Fact         `json:"facts,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
}
