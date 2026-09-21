package tools

import (
	"context"
	"encoding/json"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
)

type ArgSpec struct {
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

type ToolSchema struct {
	Properties map[string]ArgSpec `json:"properties"`
}

type ToolSpec struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Schema      ToolSchema `json:"schema"`
	// Timeout 覆盖 runtime 的全局工具超时；0 表示使用默认。
	// 供合成事务、慢失败探测等注定超过默认超时的工具声明。
	Timeout time.Duration `json:"-"`
}

// Tool 是 runtime 可执行的只读诊断能力。
// Schema 同时发给 LLM 做参数提示，并交给 policy 做执行前校验。
type Tool interface {
	Spec() ToolSpec
	Run(ctx context.Context, args json.RawMessage) (schema.Observation, error)
}
