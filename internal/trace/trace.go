package trace

import (
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
)

// Entry 是一次工具调用的审计记录。
// Result 会进入报告和下一轮 observation，Args/Duration/StartedAt 主要用于排查执行过程。
type Entry struct {
	Step           int
	ThoughtSummary string
	ToolName       string
	Args           map[string]any
	Result         schema.Observation
	Error          string
	Duration       time.Duration
	StartedAt      time.Time
}
