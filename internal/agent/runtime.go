package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type RuntimeConfig struct {
	MaxSteps            int
	ToolTimeout         time.Duration
	InitialObservations []schema.Observation
}

type Runtime struct {
	config    RuntimeConfig
	provider  llm.Provider
	registry  *tools.Registry
	validator *policy.Validator
	trace     trace.Store
}

func NewRuntime(config RuntimeConfig, provider llm.Provider, registry *tools.Registry, validator *policy.Validator, traceStore trace.Store) *Runtime {
	if config.MaxSteps <= 0 {
		config.MaxSteps = 8
	}
	return &Runtime{
		config:    config,
		provider:  provider,
		registry:  registry,
		validator: validator,
		trace:     traceStore,
	}
}

// Run 执行一次完整诊断循环：让 LLM 选择 action，校验 action，
// 执行只读工具并记录 trace，直到模型返回 final 或达到最大步数。
func (r *Runtime) Run(ctx context.Context, goal string) (*schema.Diagnosis, error) {
	for step := 1; step <= r.config.MaxSteps; step++ {
		entries := r.trace.List()
		// 每轮都把当前工具列表和已有 observation 重新发给 provider，
		// 让真实 LLM 或 mock provider 基于同一个 Request 契约决定下一步。
		action, err := r.provider.NextAction(ctx, llm.Request{
			Goal:         goal,
			Step:         step,
			Tools:        r.registry.List(),
			Observations: requestObservations(r.config.InitialObservations, entries),
		})
		if err != nil {
			return nil, fmt.Errorf("plan next action at step %d: %w", step, err)
		}
		if err := r.validator.ValidateAction(action); err != nil {
			return nil, fmt.Errorf("validate action at step %d: %w", step, err)
		}
		if action.IsFinal() {
			// final 不能只靠模型自述。这里要求它引用的 evidence 能回到 trace，
			// 保证最终报告基于本次实际执行过的只读检查。
			if err := r.validator.ValidateFinalEvidence(action.Final, entries); err != nil {
				return nil, fmt.Errorf("validate final evidence at step %d: %w", step, err)
			}
			return action.Final, nil
		}

		// 工具失败也会写入 trace/observation，下一轮交给 LLM 决定如何继续。
		if err := r.executeTool(ctx, step, action); err != nil {
			continue
		}
	}

	return nil, fmt.Errorf("max steps reached: %d", r.config.MaxSteps)
}

// observationsFromTrace 从内部审计 trace 中提取 LLM 可见的 observation。
// trace 的耗时、参数和审计字段不会回传给模型，避免模型把执行轨迹当成事实证据。
func observationsFromTrace(entries []trace.Entry) []schema.Observation {
	observations := make([]schema.Observation, 0, len(entries))
	for _, entry := range entries {
		if entry.Result.Tool == "" && entry.Result.Summary == "" && entry.Result.Error == "" && len(entry.Result.Data) == 0 {
			continue
		}
		observations = append(observations, entry.Result)
	}
	return observations
}

// requestObservations 合并静态目标上下文和真实工具观测结果。
// 初始 observation 只帮助模型选择工具参数，真正的诊断证据仍来自 trace。
func requestObservations(initial []schema.Observation, entries []trace.Entry) []schema.Observation {
	traceObservations := observationsFromTrace(entries)
	observations := make([]schema.Observation, 0, len(initial)+len(traceObservations))
	observations = append(observations, initial...)
	observations = append(observations, traceObservations...)
	return observations
}

// executeTool 在受控超时内运行工具，并把成功或失败都写入 trace。
// 工具失败不会中断整个诊断链路，下一步由 LLM 基于失败 observation 决策。
func (r *Runtime) executeTool(ctx context.Context, step int, action schema.Action) error {
	tool, ok := r.registry.Get(action.Tool)
	if !ok {
		return fmt.Errorf("tool %q is not registered", action.Tool)
	}

	args := map[string]any{}
	_ = json.Unmarshal(action.Args, &args)

	startedAt := time.Now()
	toolCtx := ctx
	cancel := func() {}
	if r.config.ToolTimeout > 0 {
		toolCtx, cancel = context.WithTimeout(ctx, r.config.ToolTimeout)
	}
	defer cancel()

	observation, err := tool.Run(toolCtx, action.Args)
	if err != nil {
		// 即使工具返回 error，也尽量把工具已经构造出的 observation 保留下来。
		// 例如 HTTP 连接失败、日志路径越界等失败本身也是后续诊断的证据。
		if observation.Tool == "" {
			observation.Tool = action.Tool
		}
		if observation.Summary == "" {
			observation.Summary = fmt.Sprintf("%s failed", action.Tool)
		}
		observation.Error = err.Error()
	}

	duration := time.Since(startedAt)
	if duration <= 0 {
		duration = time.Nanosecond
	}

	entry := trace.Entry{
		Step:           step,
		ThoughtSummary: action.ThoughtSummary,
		ToolName:       action.Tool,
		Args:           args,
		Result:         observation,
		Duration:       duration,
		StartedAt:      startedAt,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	// trace 是报告证据和下一轮 observation 的共同来源，因此成功/失败都必须落盘到 store。
	r.trace.Append(entry)

	if err != nil {
		return err
	}
	return nil
}
