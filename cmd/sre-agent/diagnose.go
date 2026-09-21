package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	memory "github.com/y2/go-sre-agent/internal/memory"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/report"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

// startDiagnosisRun 创建新 run id 并执行一次全新诊断。
// 参数: ctx 控制取消，opts 为诊断选项，mockScenario 为可选 mock；返回: 运行结果和诊断错误。
func startDiagnosisRun(ctx context.Context, opts diagnoseOptions, mockScenario string) (diagnoseResult, error) {
	startedAt := time.Now().UTC()
	var err error
	opts, err = prepareNewDiagnosisSession(opts, startedAt)
	if err != nil {
		return diagnoseResult{}, err
	}
	return executeDiagnosisRun(ctx, opts, runstore.NewRunID(startedAt), startedAt, nil, schema.Plan{}, nil, mockScenario)
}

// resumeDiagnosisRun 从已保存的 run state 恢复 trace，并用同一个 run id 继续诊断。
// 参数: ctx 控制取消，opts 指定 run 与覆盖项，mockScenario 为可选 mock；返回: 运行结果和诊断错误。
func resumeDiagnosisRun(ctx context.Context, opts resumeOptions, mockScenario string) (diagnoseResult, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return diagnoseResult{}, fmt.Errorf("run id is required")
	}
	runDir, err := resolveRunDir(opts.ConfigPath, opts.RunDir)
	if err != nil {
		return diagnoseResult{}, err
	}
	store := runstore.NewStore(runDir)
	previous, err := store.Load(opts.RunID)
	if err != nil {
		return diagnoseResult{}, err
	}
	if previous.RunID != opts.RunID {
		return diagnoseResult{}, fmt.Errorf("run %q contains mismatched run_id %q", opts.RunID, previous.RunID)
	}
	if previous.CreatedAt.IsZero() {
		return diagnoseResult{}, fmt.Errorf("run %q is missing created_at", opts.RunID)
	}
	if strings.TrimSpace(previous.Goal) == "" {
		return diagnoseResult{}, fmt.Errorf("run %q is missing goal", opts.RunID)
	}
	if previous.Status == runstore.StatusCompleted {
		if previous.Diagnosis == nil {
			return diagnoseResult{}, fmt.Errorf("completed run %q is missing diagnosis", opts.RunID)
		}
		return diagnoseResult{}, fmt.Errorf("run %q is already completed; use report instead", opts.RunID)
	}
	if previous.Status == runstore.StatusRunning && !opts.ResumeRunning {
		return diagnoseResult{}, fmt.Errorf("run %q is still marked running; confirm its original process has stopped and retry with --resume-running", opts.RunID)
	}
	if !resumableStatus(previous.Status) {
		return diagnoseResult{}, fmt.Errorf("run %q has unsupported status %q", opts.RunID, previous.Status)
	}
	if previous.Diagnosis != nil {
		return diagnoseResult{}, fmt.Errorf("incomplete run %q unexpectedly contains diagnosis", opts.RunID)
	}
	if runstore.MarkInterruptedCallsUnknown(previous.Calls, time.Now().UTC()) {
		previous.UpdatedAt = time.Now().UTC()
		if err := store.Save(previous); err != nil {
			return diagnoseResult{}, fmt.Errorf("checkpoint interrupted calls: %w", err)
		}
	}
	if runstore.HasUnknownSideEffect(previous.Calls) {
		return diagnoseResult{}, fmt.Errorf("run %q contains a side-effecting call with unknown outcome; automatic resume is blocked, inspect the target and start a new run if another probe is required", opts.RunID)
	}

	environment := opts.Environment
	if strings.TrimSpace(previous.Environment) != "" {
		environment = previous.Environment
	}
	return executeDiagnosisRun(ctx, diagnoseOptions{
		Goal:                   previous.Goal,
		ConfigPath:             opts.ConfigPath,
		Service:                previous.Service,
		MaxSteps:               opts.MaxSteps,
		LLMTimeout:             opts.LLMTimeout,
		ToolTimeout:            opts.ToolTimeout,
		TaskTimeout:            opts.TaskTimeout,
		MaxToolCalls:           opts.MaxToolCalls,
		MaxParallelTools:       opts.MaxParallelTools,
		ContextBudgetBytes:     opts.ContextBudgetBytes,
		ToolOutputBudgetBytes:  opts.ToolOutputBudgetBytes,
		Progress:               opts.Progress,
		RunDir:                 runDir,
		SessionID:              previous.SessionID,
		SessionDir:             opts.SessionDir,
		MemoryDir:              opts.MemoryDir,
		Environment:            environment,
		OverwriteSessionMemory: opts.OverwriteSessionMemory,
	}, previous.RunID, previous.CreatedAt, previous.Trace, previous.Plan, previous.Calls, mockScenario)
}

// resumableStatus 判断终态或陈旧运行态在通过恢复校验后能否安全进入新的执行尝试。
func resumableStatus(status runstore.Status) bool {
	switch status {
	case runstore.StatusFailed, runstore.StatusCancelled, runstore.StatusTimedOut, runstore.StatusRunning:
		return true
	default:
		return false
	}
}

// executeDiagnosisRun 初始化 runtime，执行诊断并组装最终运行状态。
// 参数: ctx 控制取消，opts 配置执行，runID/createdAt/existingTrace/existingPlan 恢复运行状态，mockScenario 为可选 mock；返回: 运行结果和错误。
func executeDiagnosisRun(ctx context.Context, opts diagnoseOptions, runID string, createdAt time.Time, existingTrace []trace.Entry, existingPlan schema.Plan, existingCalls []runstore.Call, mockScenario string) (diagnoseResult, error) {
	setup, err := initializeDiagnosis(opts, mockScenario)
	if err != nil {
		return diagnoseResult{RunDir: setup.config.RunDir, SessionDir: setup.config.SessionDir, Service: setup.config.Service, MemoryDir: setup.config.MemoryDir, Environment: setup.config.Environment, OverwriteSessionMemory: setup.config.OverwriteSessionMemory, ReportDir: setup.config.ReportDir}, err
	}
	cfg := setup.config
	memories, err := sessionMemoryHintsForDiagnose(cfg.SessionDir, cfg.SessionID, cfg.Environment, cfg.NewSession, cfg.OverwriteSessionMemory)
	if err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, SessionDir: cfg.SessionDir, Service: cfg.Service, MemoryDir: cfg.MemoryDir, Environment: cfg.Environment, OverwriteSessionMemory: cfg.OverwriteSessionMemory, ReportDir: cfg.ReportDir}, err
	}
	if cfg.MemoryDir != "" {
		history, err := memory.NewStore(cfg.MemoryDir).Hints(memory.Query{
			Service:     cfg.Service,
			Environment: cfg.Environment,
			Goal:        cfg.Goal,
			MaxMatches:  3,
			MaxBytes:    12 * 1024,
		})
		if err != nil {
			return diagnoseResult{RunDir: cfg.RunDir, SessionDir: cfg.SessionDir, Service: cfg.Service, MemoryDir: cfg.MemoryDir, Environment: cfg.Environment, OverwriteSessionMemory: cfg.OverwriteSessionMemory, ReportDir: cfg.ReportDir}, fmt.Errorf("load cross-session memories: %w", err)
		}
		memories = append(memories, history...)
	}

	taskCtx, cancel := context.WithTimeout(ctx, cfg.TaskTimeout)
	defer cancel()
	taskDeadline, _ := taskCtx.Deadline()
	taskDeadline = taskDeadline.UTC()
	state := runstore.State{
		RunID:        runID,
		Service:      cfg.Service,
		Environment:  cfg.Environment,
		SessionID:    cfg.SessionID,
		Goal:         cfg.Goal,
		Status:       runstore.StatusRunning,
		Plan:         existingPlan,
		Trace:        append([]trace.Entry(nil), existingTrace...),
		Calls:        append([]runstore.Call(nil), existingCalls...),
		TaskDeadline: &taskDeadline,
		CreatedAt:    createdAt,
		UpdatedAt:    time.Now().UTC(),
	}
	result := diagnoseResult{State: state, RunDir: cfg.RunDir, SessionDir: cfg.SessionDir, Service: cfg.Service, MemoryDir: cfg.MemoryDir, Environment: cfg.Environment, OverwriteSessionMemory: cfg.OverwriteSessionMemory, ReportDir: cfg.ReportDir}
	store := runstore.NewStore(cfg.RunDir)
	if err := store.Save(state); err != nil {
		return result, fmt.Errorf("checkpoint initial run state: %w", err)
	}
	checkpoint := func(snapshot agent.Checkpoint) error {
		state.Plan = snapshot.Plan
		state.Trace = snapshot.Trace
		state.Calls = snapshot.Calls
		state.Status = runstore.StatusRunning
		state.Error = ""
		state.ErrorClass = ""
		state.UpdatedAt = time.Now().UTC()
		return store.Save(state)
	}

	traceStore := trace.NewMemoryStoreWithEntries(existingTrace)
	// Runtime 把 provider、registry、policy 和 trace 串起来：
	// provider 决定下一步，policy 决定能不能执行，registry 执行工具，trace 留证。
	runtime := agent.NewRuntime(agent.RuntimeConfig{
		MaxSteps:              cfg.MaxSteps,
		LLMTimeout:            cfg.LLMTimeout,
		ToolTimeout:           cfg.ToolTimeout,
		MaxToolCalls:          cfg.MaxToolCalls,
		MaxParallelTools:      cfg.MaxParallelTools,
		ContextBudgetBytes:    cfg.ContextBudgetBytes,
		ToolOutputBudgetBytes: cfg.ToolOutputBudgetBytes,
		Model:                 setup.model,
		TargetContext:         targetContextForDiagnose(cfg),
		ToolArgOverrides:      toolArgOverridesForDiagnose(cfg),
		Memories:              memories,
		Progress:              cfg.Progress,
		ExistingCalls:         existingCalls,
		Checkpoint:            checkpoint,
	}, setup.provider, setup.registry, policy.NewValidator(policy.Config{
		ToolAllowlist: cfg.ToolAllowlist,
		ToolSchemas:   toolSchemasFromRegistry(setup.registry),
	}), traceStore)
	runtime.RestorePlan(existingPlan)

	diagnosis, err := runtime.Run(taskCtx, cfg.Goal)
	state.Plan = runtime.Plan()
	state.Trace = traceStore.List()
	state.Calls = runtime.Calls()
	state.UpdatedAt = time.Now().UTC()
	if err != nil {
		state.Status, state.ErrorClass = terminalRunStatus(taskCtx, err)
		state.Error = tools.RedactSensitive(err.Error())
		result.State = state
		return result, err
	}

	markdown := report.Markdown(report.Input{
		Goal:      cfg.Goal,
		Diagnosis: *diagnosis,
		Plan:      runtime.Plan(),
		Trace:     traceStore.List(),
	})
	state.Status = runstore.StatusCompleted
	state.Diagnosis = diagnosis
	result.Markdown = markdown
	result.State = state
	return result, nil
}

// terminalRunStatus 将执行错误映射为需要持久化的最终状态。
func terminalRunStatus(ctx context.Context, err error) (runstore.Status, runstore.ErrorClass) {
	class := agent.ClassifyError(ctx, err)
	switch class {
	case runstore.ErrorClassCancelled:
		return runstore.StatusCancelled, class
	case runstore.ErrorClassDeadlineExceeded:
		if ctx.Err() != nil {
			return runstore.StatusTimedOut, class
		}
	}
	return runstore.StatusFailed, class
}
