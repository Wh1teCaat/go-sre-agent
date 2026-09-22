package agent

import (
	"context"
	"fmt"
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
