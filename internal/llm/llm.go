package llm

import (
	"context"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// Request 是每个 runtime step 传给 LLM provider 的动作上下文。
type Request struct {
	Goal            string               `json:"goal"`
	Mode            string               `json:"mode,omitempty"`
	PlanningAllowed bool                 `json:"planning_allowed"`
	Step            int                  `json:"step"`
	TargetContext   map[string]any       `json:"target_context,omitempty"`
	Plan            *schema.Plan         `json:"plan,omitempty"`
	Memories        []schema.Memory      `json:"memories,omitempty"`
	Correction      string               `json:"correction,omitempty"`
	Tools           []tools.ToolSpec     `json:"tools"`
	Observations    []schema.Observation `json:"observations"`
}

// Decision 是模型对当前 step 的控制结果。NeedsPlan 只请求规划，不是 agent action。
type Decision struct {
	Action    *schema.Action
	NeedsPlan bool
}

type Provider interface {
	Plan(ctx context.Context, request Request) (*schema.Plan, error)
	Next(ctx context.Context, request Request) (Decision, error)
}
