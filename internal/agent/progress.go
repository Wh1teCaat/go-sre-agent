package agent

import (
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
)

const (
	// ProgressModelStarted 表示已保存调用状态，模型请求即将开始。
	ProgressModelStarted = "model_started"
	// ProgressModelCompleted 表示模型调用结果已保存。
	ProgressModelCompleted = "model_completed"
	// ProgressCheckStarted 表示已完成 checkpoint、即将执行一次工具检查。
	ProgressCheckStarted = "check_started"
	// ProgressCheckCompleted 表示一次工具检查已有可记录结果。
	ProgressCheckCompleted = "check_completed"
	// ProgressFinalizing 表示已接受 final action，正在生成最终诊断。
	ProgressFinalizing = "finalizing"
	// ProgressMemoryLoaded reports the exact memories passed to Runtime.
	ProgressMemoryLoaded = "memory_loaded"
)

// ProgressEvent 是 runtime 对外发布的结构化进度事件。回调接收方不得阻塞，
// 也不得修改 runtime 状态；CLI 和 TUI 只消费这些事件。
type ProgressEvent struct {
	Kind       string
	Step       int
	TotalSteps int
	PlanItemID string
	Tool       string
	CallID     string
	Message    string
	Summary    string
	Error      string
	Duration   time.Duration
	Memories   []schema.Memory
}
