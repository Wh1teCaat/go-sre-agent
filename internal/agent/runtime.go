package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type RuntimeConfig struct {
	MaxSteps         int
	LLMTimeout       time.Duration
	ToolTimeout      time.Duration
	Model            string
	TargetContext    map[string]any
	ToolArgOverrides map[string]map[string]any
	Plan             schema.Plan
	Memories         []schema.Memory
}

type Runtime struct {
	config    RuntimeConfig
	provider  llm.Provider
	registry  *tools.Registry
	validator *policy.Validator
	trace     trace.Store
	plan      schema.Plan
}

func NewRuntime(config RuntimeConfig, provider llm.Provider, registry *tools.Registry, validator *policy.Validator, traceStore trace.Store) *Runtime {
	if config.MaxSteps <= 0 {
		config.MaxSteps = 12
	}
	if config.LLMTimeout <= 0 {
		config.LLMTimeout = 30 * time.Second
	}
	return &Runtime{
		config:    config,
		provider:  provider,
		registry:  registry,
		validator: validator,
		trace:     traceStore,
		plan:      config.Plan,
	}
}

// Run 执行一次完整诊断循环：让 LLM 选择 action，校验 action，
// 执行只读工具并记录 trace，直到模型返回 final 或达到最大步数。
func (r *Runtime) Run(ctx context.Context, goal string) (*schema.Diagnosis, error) {
	startStep := nextStep(r.trace.List())
	if startStep > r.config.MaxSteps {
		return nil, fmt.Errorf("max steps %d already reached by existing trace step %d", r.config.MaxSteps, startStep-1)
	}
	for step := startStep; step <= r.config.MaxSteps; step++ {
		entries := r.trace.List()
		// 每轮都把当前工具列表和已有 observation 重新发给 provider，
		// 让真实 LLM 或 mock provider 基于同一个 Request 契约决定下一步。
		action, decision, err := r.nextAction(ctx, llm.Request{
			Goal:          goal,
			Step:          step,
			TargetContext: r.config.TargetContext,
			Plan:          currentPlan(r.plan),
			Memories:      r.config.Memories,
			Tools:         r.registry.List(),
			Observations:  observationsFromTrace(entries),
		}, entries)
		if err != nil {
			return nil, err
		}
		if action.Type == schema.ActionTypePlan {
			r.plan = mergePlan(r.plan, *action.Plan)
			r.trace.Append(decisionTraceEntry(step, action, decision, r.config.Model))
			continue
		}
		if action.IsFinal() {
			r.trace.Append(decisionTraceEntry(step, action, decision, r.config.Model))
			return action.Final, nil
		}

		// 工具失败也会写入 trace/observation，下一轮交给 LLM 决定如何继续。
		if err := r.executeTool(ctx, step, action, decision); err != nil {
			continue
		}
	}

	return nil, fmt.Errorf("max steps reached: %d", r.config.MaxSteps)
}

type decisionMeta struct {
	StartedAt time.Time
	Duration  time.Duration
	Attempts  int
}

// nextAction 对同一个 step 最多纠错一次。模型调用错误、action 参数错误和
// final 证据错误都会通过 correction 返回模型，重试不额外消耗 agent step。
func (r *Runtime) nextAction(ctx context.Context, request llm.Request, entries []trace.Entry) (schema.Action, decisionMeta, error) {
	meta := decisionMeta{StartedAt: time.Now()}
	var lastAction schema.Action
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		meta.Attempts = attempt
		callCtx, cancel := context.WithTimeout(ctx, r.config.LLMTimeout)
		startedAt := time.Now()
		action, err := r.provider.NextAction(callCtx, request)
		meta.Duration += time.Since(startedAt)
		cancel()
		lastAction = action
		providerFailed := err != nil
		if err != nil {
			lastErr = fmt.Errorf("plan next action at step %d: %w", request.Step, err)
		} else if action.Type == schema.ActionTypeToolCall {
			action, err = applyToolArgOverrides(action, r.config.ToolArgOverrides)
			lastAction = action
			if err != nil {
				lastErr = fmt.Errorf("prepare tool args at step %d: %w", request.Step, err)
			}
		}
		if err == nil {
			if err = r.validator.ValidateAction(action); err != nil {
				lastErr = fmt.Errorf("validate action at step %d: %w", request.Step, err)
			} else if action.IsFinal() {
				if err = r.validator.ValidateFinalCoverage(action.Final, r.plan); err != nil {
					lastErr = fmt.Errorf("validate final coverage at step %d: %w", request.Step, err)
				} else if err = r.validator.ValidateFinalEvidence(action.Final, entries); err != nil {
					lastErr = fmt.Errorf("validate final evidence at step %d: %w", request.Step, err)
				}
			}
		}
		if err == nil {
			return action, meta, nil
		}
		if attempt == 2 {
			break
		}
		request.Correction = correctionMessage(lastErr)
		if providerFailed {
			if err := waitForRetry(ctx, 100*time.Millisecond); err != nil {
				return lastAction, meta, err
			}
		}
	}
	return lastAction, meta, lastErr
}

func correctionMessage(err error) string {
	message := tools.RedactSensitive(err.Error())
	runes := []rune(message)
	if len(runes) > 500 {
		message = string(runes[:500]) + "..."
	}
	return "上一次输出未通过校验。请根据错误修正，并且只返回一个合法 action JSON：" + message
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decisionTraceEntry(step int, action schema.Action, meta decisionMeta, model string) trace.Entry {
	return trace.Entry{
		Step:           step,
		ActionType:     action.Type,
		ThoughtSummary: action.ThoughtSummary,
		ToolName:       action.Tool,
		Model:          model,
		LLMDuration:    meta.Duration,
		LLMAttempts:    meta.Attempts,
		Duration:       meta.Duration,
		StartedAt:      meta.StartedAt,
	}
}

// Plan 返回 runtime 当前检查清单，用于持久化和报告展示。
func (r *Runtime) Plan() schema.Plan {
	return r.plan
}

func currentPlan(plan schema.Plan) *schema.Plan {
	if plan.Reason == "" && len(plan.Items) == 0 {
		return nil
	}
	copied := plan
	copied.Items = append([]schema.PlanItem(nil), plan.Items...)
	return &copied
}

func mergePlan(current schema.Plan, next schema.Plan) schema.Plan {
	if next.Reason != "" {
		current.Reason = next.Reason
	}
	index := make(map[string]int, len(current.Items))
	for i, item := range current.Items {
		index[item.ID] = i
	}
	for _, item := range next.Items {
		if i, ok := index[item.ID]; ok {
			current.Items[i] = item
			continue
		}
		index[item.ID] = len(current.Items)
		current.Items = append(current.Items, item)
	}
	return current
}

// nextStep 基于已有 trace 的最大 step 计算下一次规划的 step。
// resume 会用历史 trace 初始化 runtime 的 trace store，因此不能从 1 重新开始。
func nextStep(entries []trace.Entry) int {
	next := 1
	for _, entry := range entries {
		if entry.Step >= next {
			next = entry.Step + 1
		}
	}
	return next
}

// observationsFromTrace 从内部审计 trace 中提取 LLM 可见的 observation。
// trace 的耗时、参数和审计字段不会回传给模型，避免模型把执行轨迹当成事实证据。
func observationsFromTrace(entries []trace.Entry) []schema.Observation {
	observations := make([]schema.Observation, 0, len(entries))
	for _, entry := range entries {
		if entry.Result.Tool == "" && entry.Result.Summary == "" && entry.Result.Error == "" && len(entry.Result.Data) == 0 {
			continue
		}
		observation := entry.Result
		// step 来自 trace，而不是工具返回值；模型据此引用真实 evidence。
		observation.Step = entry.Step
		observations = append(observations, observation)
	}
	return observations
}

func applyToolArgOverrides(action schema.Action, overrides map[string]map[string]any) (schema.Action, error) {
	toolOverrides := overrides[action.Tool]
	if len(toolOverrides) == 0 {
		return action, nil
	}

	args := map[string]any{}
	if len(action.Args) > 0 {
		if err := json.Unmarshal(action.Args, &args); err != nil {
			return action, fmt.Errorf("decode tool args: %w", err)
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	for key, value := range toolOverrides {
		args[key] = value
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return action, fmt.Errorf("encode tool args: %w", err)
	}
	action.Args = encoded
	return action, nil
}

// executeTool 在受控超时内运行工具，并把成功或失败都写入 trace。
// 工具失败不会中断整个诊断链路，下一步由 LLM 基于失败 observation 决策。
func (r *Runtime) executeTool(ctx context.Context, step int, action schema.Action, decision decisionMeta) error {
	tool, ok := r.registry.Get(action.Tool)
	if !ok {
		return fmt.Errorf("tool %q is not registered", action.Tool)
	}

	args := map[string]any{}
	_ = json.Unmarshal(action.Args, &args)
	traceArgs := redactTraceArgs(args)

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
		ActionType:     action.Type,
		ThoughtSummary: action.ThoughtSummary,
		ToolName:       action.Tool,
		Model:          r.config.Model,
		LLMDuration:    decision.Duration,
		LLMAttempts:    decision.Attempts,
		Args:           traceArgs,
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

func redactTraceArgs(args map[string]any) map[string]any {
	redacted := make(map[string]any, len(args))
	for key, value := range args {
		if sensitiveArgName(key) {
			redacted[key] = "[REDACTED]"
			continue
		}
		if text, ok := value.(string); ok {
			redacted[key] = tools.RedactSensitive(text)
			continue
		}
		redacted[key] = value
	}
	return redacted
}

func sensitiveArgName(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(name), "-", "_")
	return normalized == "dsn" ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "api_key") ||
		strings.Contains(normalized, "secret")
}
