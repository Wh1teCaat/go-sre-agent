package run

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
)

// CallKind 标识 run 中记录的外部操作类型。
type CallKind string

const (
	// CallKindLLMDecision 表示请求 LLM 生成下一步动作。
	CallKindLLMDecision CallKind = "llm_decision"
	// CallKindLLMPlan 表示请求 LLM 生成诊断计划。
	CallKindLLMPlan CallKind = "llm_plan"
	// CallKindTool 表示一次诊断工具调用。
	CallKindTool CallKind = "tool"
)

// CallStatus 记录调用最后一次持久化状态。
type CallStatus string

const (
	// CallStatusRunning 在外部调用开始前写入 checkpoint。
	CallStatusRunning CallStatus = "running"
	// CallStatusSucceeded 表示调用方收到了有效结果。
	CallStatusSucceeded CallStatus = "succeeded"
	// CallStatusFailed 表示调用方收到了已知失败。
	CallStatusFailed CallStatus = "failed"
	// CallStatusCancelled 表示无副作用调用在本地被取消。
	CallStatusCancelled CallStatus = "cancelled"
	// CallStatusUnknown 表示执行可能已到达目标，但没有可靠结果。
	CallStatusUnknown CallStatus = "unknown"
)

// ErrorClass 是失败调用或 run 的稳定、机器可读分类。
type ErrorClass string

const (
	// ErrorClassCancelled 标识本地取消。
	ErrorClassCancelled ErrorClass = "cancelled"
	// ErrorClassDeadlineExceeded 标识已耗尽的超时或任务预算。
	ErrorClassDeadlineExceeded ErrorClass = "deadline_exceeded"
	// ErrorClassTransient 标识可有限重试的传输失败。
	ErrorClassTransient ErrorClass = "transient"
	// ErrorClassValidation 标识无效 provider 输出。
	ErrorClassValidation ErrorClass = "validation"
	// ErrorClassPermanent 标识不会自动重试的失败。
	ErrorClassPermanent ErrorClass = "permanent"
	// ErrorClassUnknown 标识无法安全确定的执行结果。
	ErrorClassUnknown ErrorClass = "unknown"
)

// Call 是一次外部调用的持久化审计记录。
// Args、Result 和 Error 均由 runtime 在持久化前脱敏。
type Call struct {
	CallID     string              `json:"call_id"`
	Kind       CallKind            `json:"kind"`
	Step       int                 `json:"step,omitempty"`
	Attempt    int                 `json:"attempt,omitempty"`
	ToolName   string              `json:"tool_name,omitempty"`
	Args       map[string]any      `json:"args,omitempty"`
	SideEffect bool                `json:"side_effect,omitempty"`
	Status     CallStatus          `json:"status"`
	ErrorClass ErrorClass          `json:"error_class,omitempty"`
	Error      string              `json:"error,omitempty"`
	Result     *schema.Observation `json:"result,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
}

// NewCallID 为一次调用 checkpoint 创建抗碰撞标识。
func NewCallID(now time.Time) string {
	now = now.UTC()
	return "call_" + now.Format("20060102_150405") + "_" + strconv.FormatInt(now.UnixNano(), 36) + "_" + randomCallIDSuffix()
}

// MarkInterruptedCallsUnknown 将崩溃前执行中的调用转换为明确的 unknown 结果；至少
// 有一条记录变化时返回 true。
func MarkInterruptedCallsUnknown(calls []Call, now time.Time) bool {
	changed := false
	for i := range calls {
		if calls[i].Status != CallStatusRunning {
			continue
		}
		calls[i].Status = CallStatusUnknown
		calls[i].ErrorClass = ErrorClassUnknown
		calls[i].Error = "execution outcome is unknown after interruption"
		finishedAt := now.UTC()
		calls[i].FinishedAt = &finishedAt
		changed = true
	}
	return changed
}

// HasUnknownSideEffect 判断 run 是否包含可能已到达目标、但结果不可靠的有副作用工具。
func HasUnknownSideEffect(calls []Call) bool {
	for _, call := range calls {
		if call.Kind == CallKindTool && call.SideEffect && call.Status == CallStatusUnknown {
			return true
		}
	}
	return false
}

// randomCallIDSuffix 生成不可预测的后缀；随机源失败时，时间戳回退可保证持久化仍可用。
func randomCallIDSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}
