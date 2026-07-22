package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
	Memories         []schema.Memory
}

type Runtime struct {
	config    RuntimeConfig
	provider  llm.Provider
	registry  *tools.Registry
	validator *policy.Validator
	trace     *trace.MemoryStore
	plan      *schema.Plan
}

func NewRuntime(config RuntimeConfig, provider llm.Provider, registry *tools.Registry, validator *policy.Validator, traceStore *trace.MemoryStore) *Runtime {
	return &Runtime{
		config:    config,
		provider:  provider,
		registry:  registry,
		validator: validator,
		trace:     traceStore,
	}
}

// Run 执行一次完整诊断循环：让 LLM 选择是否更新计划以及下一步 action，
// 执行只读工具并记录 trace，直到模型返回 final 或达到最大步数。
func (r *Runtime) Run(ctx context.Context, goal string) (*schema.Diagnosis, error) {
	existingEntries := r.trace.List()
	startStep := nextStep(existingEntries)
	if startStep > r.config.MaxSteps {
		return nil, fmt.Errorf("max steps %d already reached by existing trace step %d", r.config.MaxSteps, startStep-1)
	}
	for step := startStep; step <= r.config.MaxSteps; step++ {
		entries := r.trace.List()
		observations := observationsFromTrace(entries)
		var actionMeta llmMeta

		request := llm.Request{
			Goal:          goal,
			Mode:          "decision",
			Step:          step,
			TargetContext: r.config.TargetContext,
			Plan:          clonePlan(r.plan),
			Memories:      r.config.Memories,
			Tools:         r.registry.List(),
			Observations:  observations,
		}
		decision, meta, err := r.requestValidDecision(ctx, request, entries, true)
		if err != nil {
			return nil, err
		}
		addLLMMeta(&actionMeta, meta)

		if decision.NeedsPlan {
			plan, meta, err := r.requestValidPlan(ctx, request)
			if err != nil {
				return nil, err
			}
			if plan != nil {
				r.plan = clonePlan(plan)
			}
			addLLMMeta(&actionMeta, meta)

			request.Plan = clonePlan(r.plan)
			decision, meta, err = r.requestValidDecision(ctx, request, entries, false)
			if err != nil {
				return nil, err
			}
			addLLMMeta(&actionMeta, meta)
		}

		action := *decision.Action
		switch action.Type {
		case schema.ActionTypeFinal:
			r.applyCoverage(action.Final.Coverage)
			r.trace.Append(actionTraceEntry(step, action, actionMeta, r.config.Model))
			return action.Final, nil
		case schema.ActionTypeToolCall:
			// 工具失败也会写入 trace/observation，下一轮交给 LLM 决定如何继续。
			if err := r.executeTool(ctx, step, action, actionMeta); err != nil {
				continue
			}
		}
	}

	return nil, fmt.Errorf("max steps reached: %d", r.config.MaxSteps)
}

type llmMeta struct {
	StartedAt time.Time
	Duration  time.Duration
	Attempts  int
}

func addLLMMeta(total *llmMeta, next llmMeta) {
	if total.StartedAt.IsZero() {
		total.StartedAt = next.StartedAt
	}
	total.Duration += next.Duration
	total.Attempts += next.Attempts
}

func (r *Runtime) requestValidPlan(ctx context.Context, request llm.Request) (*schema.Plan, llmMeta, error) {
	request.Mode = "plan"
	meta := llmMeta{StartedAt: time.Now()}
	var lastPlan *schema.Plan
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		meta.Attempts = attempt
		callCtx, cancel := context.WithTimeout(ctx, r.config.LLMTimeout)
		startedAt := time.Now()
		plan, err := r.provider.Plan(callCtx, request)
		meta.Duration += time.Since(startedAt)
		cancel()
		lastPlan = plan
		providerFailed := err != nil
		if err != nil {
			lastErr = fmt.Errorf("request plan at step %d: %w", request.Step, err)
		} else if plan == nil && request.Plan == nil {
			err = fmt.Errorf("planning request requires a non-null plan")
			lastErr = fmt.Errorf("validate plan at step %d: %w", request.Step, err)
		} else if plan != nil {
			if err = r.validator.ValidatePlan(*plan); err != nil {
				lastErr = fmt.Errorf("validate plan at step %d: %w", request.Step, err)
			}
		}
		if err == nil {
			return plan, meta, nil
		}
		if attempt == 2 {
			break
		}
		request.Correction = correctionMessage(lastErr)
		if providerFailed {
			if err := waitForRetry(ctx, 100*time.Millisecond); err != nil {
				return lastPlan, meta, err
			}
		}
	}
	return lastPlan, meta, lastErr
}

// requestValidDecision 对同一个 step 最多纠错一次，重试不额外消耗 agent step。
func (r *Runtime) requestValidDecision(ctx context.Context, request llm.Request, entries []trace.Entry, allowPlanning bool) (llm.Decision, llmMeta, error) {
	request.PlanningAllowed = allowPlanning
	meta := llmMeta{StartedAt: time.Now()}
	var lastDecision llm.Decision
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		meta.Attempts = attempt
		callCtx, cancel := context.WithTimeout(ctx, r.config.LLMTimeout)
		startedAt := time.Now()
		decision, err := r.provider.Next(callCtx, request)
		meta.Duration += time.Since(startedAt)
		cancel()
		lastDecision = decision
		providerFailed := err != nil
		if err != nil {
			lastErr = fmt.Errorf("request next decision at step %d: %w", request.Step, err)
		} else if decision.NeedsPlan {
			switch {
			case decision.Action != nil:
				err = fmt.Errorf("planning decision must not include an action")
			case !allowPlanning:
				err = fmt.Errorf("planning is already handled for this step")
			}
			if err != nil {
				lastErr = fmt.Errorf("validate decision at step %d: %w", request.Step, err)
			}
		} else if decision.Action == nil {
			err = fmt.Errorf("decision requires an action")
			lastErr = fmt.Errorf("validate decision at step %d: %w", request.Step, err)
		} else {
			action := *decision.Action
			if action.Type == schema.ActionTypeToolCall {
				action, err = applyToolArgOverrides(action, r.config.ToolArgOverrides)
				decision.Action = &action
				lastDecision = decision
				if err != nil {
					lastErr = fmt.Errorf("prepare tool args at step %d: %w", request.Step, err)
				}
			}
			if err == nil {
				if err = r.validator.ValidateAction(action); err != nil {
					lastErr = fmt.Errorf("validate action at step %d: %w", request.Step, err)
				} else if err = r.validator.ValidatePlanAdherence(action, r.plan); err != nil {
					lastErr = fmt.Errorf("validate plan adherence at step %d: %w", request.Step, err)
				} else if err = validateNoDuplicateToolCall(action, entries); err != nil {
					lastErr = fmt.Errorf("validate tool call at step %d: %w", request.Step, err)
				} else if action.Type == schema.ActionTypeFinal {
					if err = r.validator.ValidateFinalCoverage(action.Final, r.Plan()); err != nil {
						lastErr = fmt.Errorf("validate final coverage at step %d: %w", request.Step, err)
					} else if err = r.validator.ValidateFinalEvidence(action.Final, entries); err != nil {
						lastErr = fmt.Errorf("validate final evidence at step %d: %w", request.Step, err)
					}
				}
			}
		}
		if err == nil {
			return decision, meta, nil
		}
		if attempt == 2 {
			break
		}
		request.Correction = correctionMessage(lastErr)
		if providerFailed {
			if err := waitForRetry(ctx, 100*time.Millisecond); err != nil {
				return lastDecision, meta, err
			}
		}
	}
	return lastDecision, meta, lastErr
}

func correctionMessage(err error) string {
	message := tools.RedactSensitive(err.Error())
	runes := []rune(message)
	if len(runes) > 500 {
		message = string(runes[:500]) + "..."
	}
	return message
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

func actionTraceEntry(step int, action schema.Action, meta llmMeta, model string) trace.Entry {
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
	if r.plan == nil {
		return schema.Plan{}
	}
	return *clonePlan(r.plan)
}

// RestorePlan 恢复中断运行的计划；新运行的计划仍由 Provider 生成。
func (r *Runtime) RestorePlan(plan schema.Plan) {
	if plan.Reason == "" && len(plan.Items) == 0 {
		return
	}
	r.plan = clonePlan(&plan)
}

func clonePlan(plan *schema.Plan) *schema.Plan {
	if plan == nil {
		return nil
	}
	copied := *plan
	copied.Items = append([]schema.PlanItem(nil), plan.Items...)
	return &copied
}

func (r *Runtime) applyCoverage(coverage []schema.CoverageItem) {
	if r.plan == nil {
		return
	}
	statuses := make(map[string]string, len(coverage))
	for _, item := range coverage {
		statuses[strings.TrimSpace(item.PlanItemID)] = item.Status
	}
	for i := range r.plan.Items {
		if status := statuses[strings.TrimSpace(r.plan.Items[i].ID)]; status != "" {
			r.plan.Items[i].Status = status
		}
	}
}

// nextStep 基于已有 trace 的最后一个 step 计算下一次规划的 step。
// resume 会用历史 trace 初始化 runtime 的 trace store，因此不能从 1 重新开始。
func nextStep(entries []trace.Entry) int {
	if len(entries) == 0 {
		return 1
	}
	return entries[len(entries)-1].Step + 1
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
		observation.PlanItemID = entry.PlanItemID
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

func validateNoDuplicateToolCall(action schema.Action, entries []trace.Entry) error {
	if action.Type != schema.ActionTypeToolCall {
		return nil
	}
	args := map[string]any{}
	if err := json.Unmarshal(action.Args, &args); err != nil {
		return err
	}
	candidate := redactTraceArgs(args)
	for _, entry := range entries {
		if entry.ToolName == action.Tool && entry.Error == "" && reflect.DeepEqual(entry.Args, candidate) {
			// ponytail: this one-shot runtime blocks exact successful repeats; add an explicit
			// repeat intent/fingerprint if deliberate repeated measurements become a feature.
			return fmt.Errorf("duplicate successful tool call matches step %d", entry.Step)
		}
	}
	return nil
}

// executeTool 在受控超时内运行工具，并把成功或失败都写入 trace。
// 工具失败不会中断整个诊断链路，下一步由 LLM 基于失败 observation 决策。
func (r *Runtime) executeTool(ctx context.Context, step int, action schema.Action, meta llmMeta) error {
	tool, ok := r.registry.Get(action.Tool)
	if !ok {
		return fmt.Errorf("tool %q is not registered", action.Tool)
	}

	args := map[string]any{}
	_ = json.Unmarshal(action.Args, &args)
	traceArgs := redactTraceArgs(args)

	startedAt := time.Now()
	toolCtx := ctx
	if r.config.ToolTimeout > 0 {
		var cancel context.CancelFunc
		toolCtx, cancel = context.WithTimeout(ctx, r.config.ToolTimeout)
		defer cancel()
	}

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
		observation.Error = tools.RedactSensitive(err.Error())
	}
	observation = redactObservation(observation)

	duration := time.Since(startedAt)
	if duration <= 0 {
		duration = time.Nanosecond
	}

	entry := trace.Entry{
		Step:           step,
		ActionType:     action.Type,
		ThoughtSummary: action.ThoughtSummary,
		PlanItemID:     strings.TrimSpace(action.PlanItemID),
		ToolName:       action.Tool,
		Model:          r.config.Model,
		LLMDuration:    meta.Duration,
		LLMAttempts:    meta.Attempts,
		Args:           traceArgs,
		Result:         observation,
		Duration:       duration,
		StartedAt:      startedAt,
	}
	if err != nil {
		entry.Error = tools.RedactSensitive(err.Error())
	}
	// trace 是报告证据和下一轮 observation 的共同来源，因此成功/失败都必须落盘到 store。
	r.trace.Append(entry)

	if err != nil {
		return err
	}
	return nil
}

func redactObservation(observation schema.Observation) schema.Observation {
	observation.Summary = tools.RedactSensitive(observation.Summary)
	observation.Error = tools.RedactSensitive(observation.Error)
	if observation.Data != nil {
		if data, ok := tools.RedactSensitiveValue(observation.Data).(map[string]any); ok {
			observation.Data = data
		}
	}
	return observation
}

func redactTraceArgs(args map[string]any) map[string]any {
	redacted, ok := tools.RedactSensitiveValue(args).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return redacted
}
