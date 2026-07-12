package llm

import (
	"context"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// Request 是每个 runtime step 传给 LLM provider 的 Agent 级规划上下文，
// 只包含 planner 可以使用的目标、工具和已观测证据。
type Request struct {
	Goal          string               `json:"goal"`
	Step          int                  `json:"step"`
	TargetContext map[string]any       `json:"target_context,omitempty"`
	Plan          *schema.Plan         `json:"plan,omitempty"`
	Memories      []schema.Memory      `json:"memories,omitempty"`
	Correction    string               `json:"correction,omitempty"`
	Tools         []tools.ToolSpec     `json:"tools"`
	Observations  []schema.Observation `json:"observations"`
}

// Provider 是 Agent runtime 依赖的边界。实现方负责决定下一步结构化
// action，runtime 不感知具体模型 API。
type Provider interface {
	NextAction(ctx context.Context, request Request) (schema.Action, error)
}
