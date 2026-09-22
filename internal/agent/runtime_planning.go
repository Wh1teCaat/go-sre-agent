package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type llmMeta struct {
	StartedAt time.Time
	Duration  time.Duration
	Attempts  int
	CallID    string
}

func addLLMMeta(total *llmMeta, next llmMeta) {
	if total.StartedAt.IsZero() {
		total.StartedAt = next.StartedAt
	}
	total.Duration += next.Duration
	total.Attempts += next.Attempts
	if next.CallID != "" {
		total.CallID = next.CallID
	}
}

func (r *Runtime) requestValidPlan(ctx context.Context, request llm.Request) (*schema.Plan, llmMeta, error) {
	meta := llmMeta{StartedAt: time.Now()}
	var lastPlan *schema.Plan
	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		meta.Attempts = attempt
		callIndex, err := r.startCall(runstore.Call{
			Kind:    runstore.CallKindLLMPlan,
			Step:    request.Step,
			Attempt: attempt,
		})
		if err != nil {
			return lastPlan, meta, err
		}
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
			if checkpointErr := r.finishCall(callIndex, runstore.CallStatusSucceeded, "", nil, nil); checkpointErr != nil {
				return lastPlan, meta, checkpointErr
			}
			meta.CallID = r.calls[callIndex].CallID
			return plan, meta, nil
		}
		class := runstore.ErrorClassValidation
		if providerFailed {
			class = ClassifyError(ctx, err)
		}
		if checkpointErr := r.finishCall(callIndex, callStatusForError(class), class, lastErr, nil); checkpointErr != nil {
			return lastPlan, meta, checkpointErr
		}
		if attempt == 2 {
			break
		}
		request.Correction = correctionMessage(lastErr)
		if shouldRetryCall(class) {
			if err := waitForRetry(ctx, 100*time.Millisecond); err != nil {
				return lastPlan, meta, err
			}
		} else {
			break
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
		callIndex, err := r.startCall(runstore.Call{
			Kind:    runstore.CallKindLLMDecision,
			Step:    request.Step,
			Attempt: attempt,
		})
		if err != nil {
			return lastDecision, meta, err
		}
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
			switch action.Type {
			case schema.ActionTypeToolCall:
				action, err = applyToolArgOverrides(action, r.config.ToolArgOverrides)
			case schema.ActionTypeToolCalls:
				action.ToolCalls, err = applyToolCallsArgOverrides(action.ToolCalls, r.config.ToolArgOverrides)
			}
			decision.Action = &action
			lastDecision = decision
			if err != nil {
				lastErr = fmt.Errorf("prepare tool args at step %d: %w", request.Step, err)
			}
			if err == nil {
				if err = r.validator.ValidateAction(action); err != nil {
					lastErr = fmt.Errorf("validate action at step %d: %w", request.Step, err)
				} else if err = r.validateActionPlanAdherence(action); err != nil {
					lastErr = fmt.Errorf("validate plan adherence at step %d: %w", request.Step, err)
				} else if err = r.validateParallelToolCallsSafety(action); err != nil {
					lastErr = fmt.Errorf("validate parallel tool calls at step %d: %w", request.Step, err)
				} else if err = validateNoDuplicateToolCalls(toolCallsForAction(action), entries); err != nil {
					lastErr = fmt.Errorf("validate tool call at step %d: %w", request.Step, err)
				} else if err = r.validateToolCallBudget(toolCallsForAction(action)); err != nil {
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
			if checkpointErr := r.finishCall(callIndex, runstore.CallStatusSucceeded, "", nil, nil); checkpointErr != nil {
				return lastDecision, meta, checkpointErr
			}
			meta.CallID = r.calls[callIndex].CallID
			return decision, meta, nil
		}
		class := runstore.ErrorClassValidation
		if providerFailed {
			class = ClassifyError(ctx, err)
		}
		if checkpointErr := r.finishCall(callIndex, callStatusForError(class), class, lastErr, nil); checkpointErr != nil {
			return lastDecision, meta, checkpointErr
		}
		if attempt == 2 {
			break
		}
		request.Correction = correctionMessage(lastErr)
		if shouldRetryCall(class) {
			if err := waitForRetry(ctx, 100*time.Millisecond); err != nil {
				return lastDecision, meta, err
			}
		} else {
			break
		}
	}
	return lastDecision, meta, lastErr
}

// validateActionPlanAdherence 根据单个或批量 action 选择相应的计划归属校验。
func (r *Runtime) validateActionPlanAdherence(action schema.Action) error {
	switch action.Type {
	case schema.ActionTypeToolCalls:
		return r.validator.ValidateToolCallsAdherence(action.ToolCalls, r.plan)
	default:
		return r.validator.ValidatePlanAdherence(action, r.plan)
	}
}

// validateParallelToolCallsSafety 禁止把可能改变目标状态的工具放进并行批次。
// 这类调用即使独立，也必须保持阶段二定义的单调用恢复边界。
func (r *Runtime) validateParallelToolCallsSafety(action schema.Action) error {
	if action.Type != schema.ActionTypeToolCalls {
		return nil
	}
	for _, call := range action.ToolCalls {
		if tool, ok := r.registry.Get(call.Tool); ok && tool.Spec().SideEffect {
			return fmt.Errorf("side-effecting tool %q cannot be in parallel tool_calls", call.Tool)
		}
	}
	return nil
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

// timeoutError 只保留现代网络错误分类实际需要的超时契约，不依赖已废弃且语义不稳定的
// net.Error.Temporary。
type timeoutError interface {
	Timeout() bool
}

// ClassifyError 将执行失败映射为稳定状态值。父 context 取消始终优先于子调用超时，
// 因为它表示任务被主动停止或总任务预算已耗尽。
func ClassifyError(ctx context.Context, err error) runstore.ErrorClass {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return runstore.ErrorClassCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return runstore.ErrorClassDeadlineExceeded
	}
	var timeoutErr timeoutError
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return runstore.ErrorClassTransient
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "status 5") || strings.Contains(message, "status 429") || strings.Contains(message, "status 408") {
		return runstore.ErrorClassTransient
	}
	return runstore.ErrorClassPermanent
}

// callStatusForError 为非工具调用失败选择持久化状态。
func callStatusForError(class runstore.ErrorClass) runstore.CallStatus {
	if class == runstore.ErrorClassCancelled {
		return runstore.CallStatusCancelled
	}
	return runstore.CallStatusFailed
}

// shouldRetryCall 将重试限制在纠错和临时传输失败路径。
func shouldRetryCall(class runstore.ErrorClass) bool {
	return class == runstore.ErrorClassValidation || class == runstore.ErrorClassTransient || class == runstore.ErrorClassDeadlineExceeded
}

func actionTraceEntry(step int, action schema.Action, meta llmMeta, model string) trace.Entry {
	return trace.Entry{
		Step:           step,
		CallID:         meta.CallID,
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

// applyToolCallsArgOverrides 对批量工具调用逐项应用受配置保护的目标参数。
func applyToolCallsArgOverrides(calls []schema.ToolCall, overrides map[string]map[string]any) ([]schema.ToolCall, error) {
	prepared := append([]schema.ToolCall(nil), calls...)
	for index := range prepared {
		action, err := applyToolArgOverrides(schema.Action{
			Type: schema.ActionTypeToolCall,
			Tool: prepared[index].Tool,
			Args: prepared[index].Args,
		}, overrides)
		if err != nil {
			return nil, err
		}
		prepared[index].Args = action.Args
	}
	return prepared, nil
}

// toolCallsForAction 将单个或批量 action 统一为工具调用列表，便于预算与重复检查。
func toolCallsForAction(action schema.Action) []schema.ToolCall {
	switch action.Type {
	case schema.ActionTypeToolCall:
		return []schema.ToolCall{{
			ThoughtSummary: action.ThoughtSummary,
			PlanItemID:     action.PlanItemID,
			Tool:           action.Tool,
			Args:           action.Args,
		}}
	case schema.ActionTypeToolCalls:
		return append([]schema.ToolCall(nil), action.ToolCalls...)
	default:
		return nil
	}
}

// validateNoDuplicateToolCalls 防止新调用与既有成功调用重复，也防止同一批次
// 通过不同顺序重复相同参数。并行批量不允许依赖执行顺序。
func validateNoDuplicateToolCalls(calls []schema.ToolCall, entries []trace.Entry) error {
	seen := map[string]struct{}{}
	for _, call := range calls {
		args := map[string]any{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return err
		}
		candidate := redactTraceArgs(args)
		keyData, err := json.Marshal(struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		}{Tool: call.Tool, Args: candidate})
		if err != nil {
			return err
		}
		key := string(keyData)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate tool call %q in one action", call.Tool)
		}
		seen[key] = struct{}{}
		for _, entry := range entries {
			if entry.ToolName == call.Tool && entry.Error == "" && reflect.DeepEqual(entry.Args, candidate) {
				return fmt.Errorf("duplicate successful tool call matches step %d", entry.Step)
			}
		}
	}
	return nil
}
