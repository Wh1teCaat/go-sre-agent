package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

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
