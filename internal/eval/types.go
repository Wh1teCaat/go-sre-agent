// eval 包为 SRE agent 提供确定性回归场景和结果存储。它刻意不依赖命令行配置
// 或真实 LLM 客户端，因此 mock runner 可安全用于测试和离线评测。
package eval

import (
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// Mode 标识生成 Result 所使用的评测器。
type Mode string

const (
	// ModeMock 是使用进程内 mock provider 和样本工具的确定性运行；它绝不连接模型
	// 或诊断目标。
	ModeMock Mode = "mock"
	// ModeReal 预留给显式授权的真实模型评测；本包自身不会发起此类调用。
	ModeReal Mode = "real"
)

// Status 是评测结果。Skipped 被刻意与 Passed 区分，避免未执行的真实模型评测被报告为
// 成功。
type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
)

// Assertion 记录一条确定性预期及其结果。
type Assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Expectations 描述 mock 与真实模型评测器共用的检查；ToolNames 是必需 trace 工具名，
// 不是完整的 trace allowlist。
type Expectations struct {
	ToolNames []string `json:"tool_names,omitempty"`
	// ToolSequence 是 trace 中预期出现的有序子序列。允许额外检查，避免显式评测的
	// 真实模型仅因收集了额外有效证据而失败。
	ToolSequence    []string `json:"tool_sequence,omitempty"`
	SummaryContains []string `json:"summary_contains,omitempty"`
}

// ToolFixture 是一个 runtime 工具的确定性内存实现。RunMock 只从该值返回
// Observation 和 Error，不执行网络、文件系统或子进程操作。
type ToolFixture struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Schema      tools.ToolSchema   `json:"schema"`
	Observation schema.Observation `json:"observation"`
	Error       string             `json:"error,omitempty"`
}

// Scenario 是固定动作序列和确定性工具 observation 的组合；可提供给 RunMock，或
// 用作单独授权的真实模型运行预期。
type Scenario struct {
	ID           string          `json:"id"`
	Version      string          `json:"version,omitempty"`
	Name         string          `json:"name"`
	Goal         string          `json:"goal"`
	MaxSteps     int             `json:"max_steps"`
	Actions      []schema.Action `json:"actions"`
	Tools        []ToolFixture   `json:"tools,omitempty"`
	Expectations Expectations    `json:"expectations"`
}

// Result 是单次评测可脱敏、可持久化的记录。确定性 mock 运行不会创建诊断 run，因此
// RunID 可选；显式授权真实评测的调用方可设置它。
type Result struct {
	ID                string      `json:"id"`
	ScenarioID        string      `json:"scenario_id"`
	ScenarioName      string      `json:"scenario_name,omitempty"`
	ScenarioVersion   string      `json:"scenario_version,omitempty"`
	RunID             string      `json:"run_id,omitempty"`
	Command           string      `json:"command,omitempty"`
	Mode              Mode        `json:"mode"`
	Status            Status      `json:"status"`
	Model             string      `json:"model,omitempty"`
	ExecutedRealModel bool        `json:"executed_real_model"`
	StartedAt         time.Time   `json:"started_at"`
	FinishedAt        time.Time   `json:"finished_at"`
	DurationMS        int64       `json:"duration_ms"`
	TraceSteps        int         `json:"trace_steps,omitempty"`
	Assertions        []Assertion `json:"assertions,omitempty"`
	Error             string      `json:"error,omitempty"`
}
