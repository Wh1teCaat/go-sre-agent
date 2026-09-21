package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type RuntimeConfig struct {
	MaxSteps              int
	LLMTimeout            time.Duration
	ToolTimeout           time.Duration
	MaxToolCalls          int
	MaxParallelTools      int
	ContextBudgetBytes    int
	ToolOutputBudgetBytes int
	Model                 string
	TargetContext         map[string]any
	ToolArgOverrides      map[string]map[string]any
	Memories              []schema.Memory
	// Progress 接收非阻塞的运行时进度事件；它不参与持久化或决策。
	Progress func(ProgressEvent)
	// ExistingCalls 在恢复尝试前还原持久化调用记录。
	ExistingCalls []runstore.Call
	// Checkpoint 在每次外部调用前后持久化 runtime 快照；返回错误会阻止下一次外部
	// 调用开始。
	Checkpoint func(Checkpoint) error
}

// Checkpoint 是安全恢复所需的 runtime 状态持久化子集。
type Checkpoint struct {
	Plan  schema.Plan
	Trace []trace.Entry
	Calls []runstore.Call
}

type Runtime struct {
	config    RuntimeConfig
	provider  llm.Provider
	registry  *tools.Registry
	validator *policy.Validator
	trace     *trace.MemoryStore
	plan      *schema.Plan
	calls     []runstore.Call
}

func NewRuntime(config RuntimeConfig, provider llm.Provider, registry *tools.Registry, validator *policy.Validator, traceStore *trace.MemoryStore) *Runtime {
	config = normalizeRuntimeConfig(config)
	return &Runtime{
		config:    config,
		provider:  provider,
		registry:  registry,
		validator: validator,
		trace:     traceStore,
		calls:     append([]runstore.Call(nil), config.ExistingCalls...),
	}
}

const (
	defaultContextBudgetBytes    = 48 * 1024
	defaultToolOutputBudgetBytes = 8 * 1024
)

// normalizeRuntimeConfig 为未通过 CLI 初始化的测试和嵌入式调用提供保守默认值。
func normalizeRuntimeConfig(config RuntimeConfig) RuntimeConfig {
	if config.MaxToolCalls <= 0 {
		config.MaxToolCalls = config.MaxSteps
	}
	if config.MaxParallelTools <= 0 {
		config.MaxParallelTools = 1
	}
	if config.ContextBudgetBytes <= 0 {
		config.ContextBudgetBytes = defaultContextBudgetBytes
	}
	if config.ToolOutputBudgetBytes <= 0 {
		config.ToolOutputBudgetBytes = defaultToolOutputBudgetBytes
	}
	return config
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries := r.trace.List()
		observations := observationsForPrompt(entries, r.config.ContextBudgetBytes, r.config.ToolOutputBudgetBytes)
		var actionMeta llmMeta

		request := llm.Request{
			Goal:          goal,
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
			r.emitProgress(ProgressEvent{Kind: ProgressFinalizing, Step: step, TotalSteps: r.config.MaxSteps})
			r.applyCoverage(action.Final.Coverage)
			r.trace.Append(actionTraceEntry(step, action, actionMeta, r.config.Model))
			if err := r.checkpoint(); err != nil {
				return nil, fmt.Errorf("checkpoint final action: %w", err)
			}
			return action.Final, nil
		case schema.ActionTypeToolCall:
			// 工具失败也会写入 trace/observation，下一轮交给 LLM 决定如何继续。
			if err := r.executeTool(ctx, step, action, actionMeta); err != nil {
				return nil, err
			}
		case schema.ActionTypeToolCalls:
			if err := r.executeToolCalls(ctx, step, action.ToolCalls, actionMeta); err != nil {
				return nil, err
			}
		}
	}

	return nil, fmt.Errorf("max steps reached: %d", r.config.MaxSteps)
}

// emitProgress 将进度发布在 runtime 边界，避免 CLI 输出影响诊断状态或模型上下文。
func (r *Runtime) emitProgress(event ProgressEvent) {
	if r.config.Progress != nil {
		r.config.Progress(event)
	}
}

// validateToolCallBudget 确保批量 action 也不会绕过单次运行的工具调用总预算。
func (r *Runtime) validateToolCallBudget(calls []schema.ToolCall) error {
	used := 0
	for _, call := range r.calls {
		if call.Kind == runstore.CallKindTool {
			used++
		}
	}
	if used+len(calls) > r.config.MaxToolCalls {
		return fmt.Errorf("tool call budget exceeded: used %d, requested %d, limit %d", used, len(calls), r.config.MaxToolCalls)
	}
	return nil
}

// Calls 返回 runtime 持久化外部调用记录的副本。
func (r *Runtime) Calls() []runstore.Call {
	return append([]runstore.Call(nil), r.calls...)
}

// checkpoint 发布一致的 trace、plan 与调用快照。它刻意同步执行：checkpoint 失败
// 时必须停止新的外部操作。
func (r *Runtime) checkpoint() error {
	if r.config.Checkpoint == nil {
		return nil
	}
	return r.config.Checkpoint(Checkpoint{
		Plan:  r.Plan(),
		Trace: append([]trace.Entry(nil), r.trace.List()...),
		Calls: r.Calls(),
	})
}

// startCall 在外部请求开始前写入 running 调用记录。
func (r *Runtime) startCall(call runstore.Call) (int, error) {
	call.CallID = runstore.NewCallID(time.Now())
	call.Status = runstore.CallStatusRunning
	call.StartedAt = time.Now().UTC()
	r.calls = append(r.calls, call)
	index := len(r.calls) - 1
	if err := r.checkpoint(); err != nil {
		return -1, fmt.Errorf("checkpoint call %s before execution: %w", call.CallID, err)
	}
	return index, nil
}

// finishCall 记录可观察结果，并将其与 trace 一同 checkpoint。
func (r *Runtime) finishCall(index int, status runstore.CallStatus, class runstore.ErrorClass, callErr error, result *schema.Observation) error {
	if index < 0 || index >= len(r.calls) {
		return fmt.Errorf("call checkpoint index %d is out of range", index)
	}
	call := &r.calls[index]
	call.Status = status
	call.ErrorClass = class
	if callErr != nil {
		call.Error = tools.RedactSensitive(callErr.Error())
	} else {
		call.Error = ""
	}
	if result != nil {
		copied := *result
		call.Result = &copied
	}
	finishedAt := time.Now().UTC()
	call.FinishedAt = &finishedAt
	if err := r.checkpoint(); err != nil {
		return fmt.Errorf("checkpoint call %s after execution: %w", call.CallID, err)
	}
	return nil
}

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

// ClassifyError 将执行失败映射为稳定状态值。父 context 取消始终优先于子调用超时，
// 因为它表示任务被主动停止或总任务预算已耗尽。
func ClassifyError(ctx context.Context, err error) runstore.ErrorClass {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return runstore.ErrorClassCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return runstore.ErrorClassDeadlineExceeded
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
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

// preparedToolCall 是已完成调用前 checkpoint、但尚未开始执行的工具调用。
type preparedToolCall struct {
	call       schema.ToolCall
	args       map[string]any
	traceArgs  map[string]any
	tool       tools.Tool
	sideEffect bool
	callIndex  int
}

// toolExecutionResult 是并行 worker 的无副作用回传值；持久化统一在主循环完成。
type toolExecutionResult struct {
	prepared    preparedToolCall
	observation schema.Observation
	err         error
	startedAt   time.Time
	duration    time.Duration
}

// executeTool 保持单工具 action 的兼容入口，实际复用批量执行和持久化逻辑。
func (r *Runtime) executeTool(ctx context.Context, step int, action schema.Action, meta llmMeta) error {
	return r.executeToolCalls(ctx, step, toolCallsForAction(action), meta)
}

// executeToolCalls 为每项调用先持久化 running checkpoint，再按并发上限执行。worker
// 不修改 trace 或 calls，结果在主循环按 action 顺序写回，保证恢复快照的一致性。
func (r *Runtime) executeToolCalls(ctx context.Context, step int, calls []schema.ToolCall, meta llmMeta) error {
	prepared := make([]preparedToolCall, 0, len(calls))
	for _, call := range calls {
		args := map[string]any{}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return fmt.Errorf("decode tool args for %q: %w", call.Tool, err)
		}
		if args == nil {
			args = map[string]any{}
		}
		tool, exists := r.registry.Get(call.Tool)
		sideEffect := exists && tool.Spec().SideEffect
		callIndex, err := r.startCall(runstore.Call{
			Kind:       runstore.CallKindTool,
			Step:       step,
			ToolName:   call.Tool,
			Args:       redactTraceArgs(args),
			SideEffect: sideEffect,
		})
		if err != nil {
			return err
		}
		prepared = append(prepared, preparedToolCall{
			call:       call,
			args:       args,
			traceArgs:  redactTraceArgs(args),
			tool:       tool,
			sideEffect: sideEffect,
			callIndex:  callIndex,
		})
	}

	results := make([]toolExecutionResult, len(prepared))
	parallelism := min(r.config.MaxParallelTools, len(prepared))
	semaphore := make(chan struct{}, parallelism)
	completed := make(chan int, len(prepared))
	for index := range prepared {
		current := prepared[index]
		callID := r.calls[current.callIndex].CallID
		r.emitProgress(ProgressEvent{
			Kind:       ProgressCheckStarted,
			Step:       step,
			TotalSteps: r.config.MaxSteps,
			PlanItemID: strings.TrimSpace(current.call.PlanItemID),
			Tool:       current.call.Tool,
			CallID:     callID,
			Message:    current.call.ThoughtSummary,
		})
		go func(index int, current preparedToolCall) {
			semaphore <- struct{}{}
			results[index] = runPreparedTool(ctx, r.config.ToolTimeout, current)
			<-semaphore
			completed <- index
		}(index, current)
	}
	for range prepared {
		<-completed
	}

	for _, result := range results {
		if err := r.recordToolExecution(ctx, step, result, meta); err != nil {
			return err
		}
	}
	return nil
}

// runPreparedTool 在 worker 中执行已 checkpoint 的工具。它不写共享状态，使并发
// 调度不会与 trace、calls 或持久化存储产生数据竞争。
func runPreparedTool(ctx context.Context, defaultTimeout time.Duration, prepared preparedToolCall) toolExecutionResult {
	result := toolExecutionResult{prepared: prepared, startedAt: time.Now()}
	if prepared.tool == nil {
		result.err = fmt.Errorf("tool %q is not registered", prepared.call.Tool)
		result.duration = time.Since(result.startedAt)
		return result
	}
	timeout := defaultTimeout
	if specTimeout := prepared.tool.Spec().Timeout; specTimeout > 0 {
		timeout = specTimeout
	}
	toolCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		toolCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	result.observation, result.err = prepared.tool.Run(toolCtx, prepared.call.Args)
	result.duration = time.Since(result.startedAt)
	return result
}

// recordToolExecution 将 worker 结果顺序写入 trace 和运行调用记录，并在完成
// checkpoint 后发布完成事件。工具错误仍是可供后续诊断使用的 observation。
func (r *Runtime) recordToolExecution(ctx context.Context, step int, execution toolExecutionResult, meta llmMeta) error {
	prepared := execution.prepared
	observation := execution.observation
	hasToolResult := hasObservationResult(observation)
	if execution.err != nil {
		if observation.Tool == "" {
			observation.Tool = prepared.call.Tool
		}
		if observation.Summary == "" {
			observation.Summary = fmt.Sprintf("%s failed", prepared.call.Tool)
		}
		observation.Error = tools.RedactSensitive(execution.err.Error())
	}
	observation = redactObservation(observation)
	observation = enrichObservation(observation, prepared.call.Tool, prepared.args, execution.startedAt, ctx, execution.err, prepared.tool != nil, hasToolResult)
	// 目标身份和 Facts 在补齐阶段才生成，因此在写入 trace 前再次经过同一脱敏边界。
	observation = redactObservation(observation)

	duration := execution.duration
	if duration <= 0 {
		duration = time.Nanosecond
	}
	entry := trace.Entry{
		Step:           step,
		CallID:         r.calls[prepared.callIndex].CallID,
		ActionType:     schema.ActionTypeToolCall,
		ThoughtSummary: prepared.call.ThoughtSummary,
		PlanItemID:     strings.TrimSpace(prepared.call.PlanItemID),
		ToolName:       prepared.call.Tool,
		Model:          r.config.Model,
		LLMDuration:    meta.Duration,
		LLMAttempts:    meta.Attempts,
		Args:           prepared.traceArgs,
		Result:         observation,
		Duration:       duration,
		StartedAt:      execution.startedAt,
	}
	if execution.err != nil {
		entry.Error = tools.RedactSensitive(execution.err.Error())
	}
	r.trace.Append(entry)

	result := observation
	status := runstore.CallStatusSucceeded
	class := runstore.ErrorClass("")
	callErr := error(nil)
	if execution.err != nil {
		callErr = execution.err
		class = ClassifyError(ctx, execution.err)
		status = callStatusForError(class)
		if prepared.sideEffect {
			status = runstore.CallStatusUnknown
			class = runstore.ErrorClassUnknown
			callErr = fmt.Errorf("execution outcome is unknown: %w", execution.err)
		}
	}
	if err := r.finishCall(prepared.callIndex, status, class, callErr, &result); err != nil {
		return err
	}
	r.emitProgress(ProgressEvent{
		Kind:       ProgressCheckCompleted,
		Step:       step,
		TotalSteps: r.config.MaxSteps,
		PlanItemID: strings.TrimSpace(prepared.call.PlanItemID),
		Tool:       prepared.call.Tool,
		CallID:     entry.CallID,
		Summary:    observation.Summary,
		Error:      entry.Error,
		Duration:   duration,
	})
	return nil
}

// redactObservation 在 observation 进入 trace、调用记录和后续模型上下文前脱敏。
func redactObservation(observation schema.Observation) schema.Observation {
	observation.Summary = tools.RedactSensitive(observation.Summary)
	observation.Error = tools.RedactSensitive(observation.Error)
	observation.Target.Kind = tools.RedactSensitive(observation.Target.Kind)
	observation.Target.ID = tools.RedactSensitive(observation.Target.ID)
	if observation.Data != nil {
		if data, ok := tools.RedactSensitiveValue(observation.Data).(map[string]any); ok {
			observation.Data = data
		}
	}
	for index := range observation.Facts {
		observation.Facts[index].Key = tools.RedactSensitive(observation.Facts[index].Key)
		observation.Facts[index].Target.Kind = tools.RedactSensitive(observation.Facts[index].Target.Kind)
		observation.Facts[index].Target.ID = tools.RedactSensitive(observation.Facts[index].Target.ID)
		observation.Facts[index].Value = tools.RedactSensitiveValue(observation.Facts[index].Value)
	}
	return observation
}

// enrichObservation 补齐 runtime 可确定的事实元数据。工具只需报告自身观测；
// 检查是否完成、观测时间和调用目标由 runtime 统一记录，避免不同工具语义漂移。
func enrichObservation(observation schema.Observation, toolName string, args map[string]any, startedAt time.Time, ctx context.Context, runErr error, invoked bool, hadResult bool) schema.Observation {
	if observation.CheckStatus == "" {
		observation.CheckStatus = checkStatus(ctx, runErr, invoked, hadResult)
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = startedAt.UTC()
	}
	if observation.Target.Kind == "" || observation.Target.ID == "" {
		observation.Target = targetIdentityForAction(toolName, args)
	}
	if observation.TargetHealth == "" {
		observation.TargetHealth = inferTargetHealth(toolName, observation)
	}
	if len(observation.Facts) == 0 {
		observation.Facts = factsFromObservation(observation)
	} else {
		for index := range observation.Facts {
			if observation.Facts[index].ObservedAt.IsZero() {
				observation.Facts[index].ObservedAt = observation.ObservedAt
			}
			if observation.Facts[index].Target.Kind == "" || observation.Facts[index].Target.ID == "" {
				observation.Facts[index].Target = observation.Target
			}
		}
	}
	return observation
}

// hasObservationResult 判断工具是否在返回错误前提供了可保留的诊断结果。
func hasObservationResult(observation schema.Observation) bool {
	return observation.Tool != "" || observation.Summary != "" || len(observation.Data) > 0 || len(observation.Facts) > 0
}

// checkStatus 只描述检查动作是否获得可用观测，绝不根据目标返回 5xx、缺表等
// 业务结果推断为失败。这样“检查完成但目标不健康”可以被精确表达。
func checkStatus(ctx context.Context, runErr error, invoked bool, hadResult bool) string {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(runErr, context.Canceled) {
		return schema.CheckExecutionCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded) {
		return schema.CheckExecutionTimedOut
	}
	if !invoked || runErr != nil || !hadResult {
		return schema.CheckExecutionFailed
	}
	return schema.CheckExecutionCompleted
}

// inferTargetHealth 只使用完成检查返回的明确协议或状态字段。连接错误、超时等
// 不能区分目标故障与诊断路径故障，因此保守地标为 unknown。
func inferTargetHealth(toolName string, observation schema.Observation) string {
	if observation.CheckStatus != schema.CheckExecutionCompleted {
		return schema.TargetHealthUnknown
	}
	data := observation.Data
	switch toolName {
	case "http_check":
		if status, ok := integerDataValue(data, "status"); ok {
			switch {
			case status >= 500:
				return schema.TargetHealthUnhealthy
			case status >= 400:
				// 401/403 等状态证明可达或被鉴权拦截，但不能证明应用健康。
				return schema.TargetHealthUnknown
			default:
				return schema.TargetHealthHealthy
			}
		}
	case "websocket_check":
		if handshake, ok := boolDataValue(data, "handshake_success"); ok {
			if !handshake {
				return schema.TargetHealthUnhealthy
			}
			if pingOK, exists := boolDataValue(data, "ping_pong_ok"); exists && !pingOK {
				return schema.TargetHealthDegraded
			}
			return schema.TargetHealthHealthy
		}
	case "postgres_ping", "postgres_check", "redis_ping", "redis_check":
		return schema.TargetHealthHealthy
	case "kafka_check":
		if exists, ok := boolDataValue(data, "topic_exists"); ok && !exists {
			return schema.TargetHealthUnhealthy
		}
		if lag, ok := integerDataValue(data, "active_lag"); ok && lag > 0 {
			return schema.TargetHealthDegraded
		}
		return schema.TargetHealthHealthy
	case "log_read", "docker_logs", "docker_stats":
		return schema.TargetHealthNotApplicable
	case "docker_inspect":
		if running, ok := boolDataValue(data, "running"); ok && !running {
			return schema.TargetHealthUnhealthy
		}
	}
	return schema.TargetHealthUnknown
}

// boolDataValue 从工具数据中读取布尔型事实。
func boolDataValue(data map[string]any, key string) (bool, bool) {
	if data == nil {
		return false, false
	}
	value, ok := data[key]
	if !ok {
		return false, false
	}
	result, ok := value.(bool)
	return result, ok
}

// integerDataValue 从工具数据中读取可表示为整数的状态值。
func integerDataValue(data map[string]any, key string) (int, bool) {
	if data == nil {
		return 0, false
	}
	value, ok := data[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		result, err := typed.Int64()
		return int(result), err == nil
	case string:
		result, err := strconv.Atoi(typed)
		return result, err == nil
	default:
		return 0, false
	}
}

// targetIdentityForAction 从工具参数提取不会泄露凭据的目标身份。不能安全解析时，
// 使用工具级标识而不是把原始参数写入 trace。
func targetIdentityForAction(toolName string, args map[string]any) schema.TargetIdentity {
	if rawURL, ok := args["url"].(string); ok && strings.TrimSpace(rawURL) != "" {
		return schema.TargetIdentity{Kind: "endpoint", ID: safeURLIdentity(rawURL)}
	}
	if toolName == "postgres_ping" || toolName == "postgres_check" {
		if dsn, ok := args["dsn"].(string); ok {
			return schema.TargetIdentity{Kind: "postgres", ID: safePostgresIdentity(dsn)}
		}
	}
	if addr, ok := args["addr"].(string); ok && strings.TrimSpace(addr) != "" {
		identity := strings.TrimSpace(addr)
		if topic, ok := args["topic"].(string); ok && strings.TrimSpace(topic) != "" {
			identity += "/" + strings.TrimSpace(topic)
		}
		return schema.TargetIdentity{Kind: "network_service", ID: identity}
	}
	if path, ok := args["path"].(string); ok && strings.TrimSpace(path) != "" {
		return schema.TargetIdentity{Kind: "log_file", ID: strings.TrimSpace(path)}
	}
	if container, ok := args["container"].(string); ok && strings.TrimSpace(container) != "" {
		identity := strings.TrimSpace(container)
		if probe, ok := args["probe"].(string); ok && strings.TrimSpace(probe) != "" {
			identity += "/" + strings.TrimSpace(probe)
		}
		return schema.TargetIdentity{Kind: "container", ID: identity}
	}
	return schema.TargetIdentity{Kind: "diagnostic_tool", ID: toolName}
}

// safeURLIdentity 去除 URL 中的用户信息、查询串和片段后再作为目标身份保存。
func safeURLIdentity(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "configured_endpoint"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// safePostgresIdentity 从 PostgreSQL DSN 提取不含凭据的地址和数据库身份。
func safePostgresIdentity(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	values := map[string]string{}
	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if key == "host" || key == "port" || key == "dbname" {
			values[key] = strings.Trim(value, "'\"")
		}
	}
	if values["host"] == "" {
		return "configured_postgres"
	}
	port := values["port"]
	if port == "" {
		port = "5432"
	}
	database := values["dbname"]
	if database == "" {
		database = "default"
	}
	return values["host"] + ":" + port + "/" + database
}

// factsFromObservation 从已脱敏的结构化工具数据提取稳定的原子事实。日志正文、
// 响应片段和命令输出保留在 Data 中，不在 Facts 重复展开，以免制造无界上下文。
func factsFromObservation(observation schema.Observation) []schema.Fact {
	facts := []schema.Fact{{
		Key:        "summary",
		Value:      observation.Summary,
		ObservedAt: observation.ObservedAt,
		Target:     observation.Target,
	}}
	keys := make([]string, 0, len(observation.Data))
	for key := range observation.Data {
		if key == "body_snippet" || key == "lines" || key == "output" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := observation.Data[key]
		if !isFactValue(value) {
			continue
		}
		facts = append(facts, schema.Fact{
			Key:        key,
			Value:      value,
			ObservedAt: observation.ObservedAt,
			Target:     observation.Target,
		})
	}
	return facts
}

// isFactValue 限制自动提升为 Facts 的值类型和字符串长度。
func isFactValue(value any) bool {
	switch typed := value.(type) {
	case nil, bool, int, int64, float64, json.Number:
		return true
	case string:
		return len([]rune(typed)) <= 512
	default:
		return false
	}
}

func redactTraceArgs(args map[string]any) map[string]any {
	redacted, ok := tools.RedactSensitiveValue(args).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return redacted
}
