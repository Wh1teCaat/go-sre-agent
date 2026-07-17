package trace

import (
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
)

// Entry 是一次已接受 action 的审计记录。
// 只有 tool_call 的 Result 会进入下一轮 observation；final 只保留决策元数据。
type Entry struct {
	Step           int
	ActionType     string
	ThoughtSummary string
	PlanItemID     string
	ToolName       string
	Model          string
	LLMDuration    time.Duration
	LLMAttempts    int
	Args           map[string]any
	Result         schema.Observation
	Error          string
	Duration       time.Duration
	StartedAt      time.Time
}
