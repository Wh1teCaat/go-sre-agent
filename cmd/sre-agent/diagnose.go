package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
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
	return executeDiagnosisRun(ctx, opts, runstore.NewRunID(startedAt), startedAt, nil, schema.Plan{}, mockScenario)
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
	if previous.Status != runstore.StatusFailed {
		return diagnoseResult{}, fmt.Errorf("run %q has unsupported status %q", opts.RunID, previous.Status)
	}
	if previous.Diagnosis != nil {
		return diagnoseResult{}, fmt.Errorf("failed run %q unexpectedly contains diagnosis", opts.RunID)
	}

	return executeDiagnosisRun(ctx, diagnoseOptions{
		Goal:                   previous.Goal,
		ConfigPath:             opts.ConfigPath,
		MaxSteps:               opts.MaxSteps,
		LLMTimeout:             opts.LLMTimeout,
		ToolTimeout:            opts.ToolTimeout,
		RunDir:                 runDir,
		SessionID:              previous.SessionID,
		SessionDir:             opts.SessionDir,
		Environment:            opts.Environment,
		OverwriteSessionMemory: opts.OverwriteSessionMemory,
	}, previous.RunID, previous.CreatedAt, previous.Trace, previous.Plan, mockScenario)
}

// executeDiagnosisRun 初始化 runtime，执行诊断并组装最终运行状态。
// 参数: ctx 控制取消，opts 配置执行，runID/createdAt/existingTrace/existingPlan 恢复运行状态，mockScenario 为可选 mock；返回: 运行结果和错误。
func executeDiagnosisRun(ctx context.Context, opts diagnoseOptions, runID string, createdAt time.Time, existingTrace []trace.Entry, existingPlan schema.Plan, mockScenario string) (diagnoseResult, error) {
	setup, err := initializeDiagnosis(opts, mockScenario)
	if err != nil {
		return diagnoseResult{RunDir: setup.config.RunDir, SessionDir: setup.config.SessionDir, Environment: setup.config.Environment, OverwriteSessionMemory: setup.config.OverwriteSessionMemory, ReportDir: setup.config.ReportDir}, err
	}
	cfg := setup.config
	memories, err := sessionMemoryHintsForDiagnose(cfg.SessionDir, cfg.SessionID, cfg.Environment, cfg.NewSession, cfg.OverwriteSessionMemory)
	if err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, SessionDir: cfg.SessionDir, Environment: cfg.Environment, OverwriteSessionMemory: cfg.OverwriteSessionMemory, ReportDir: cfg.ReportDir}, err
	}

	traceStore := trace.NewMemoryStoreWithEntries(existingTrace)
	// Runtime 把 provider、registry、policy 和 trace 串起来：
	// provider 决定下一步，policy 决定能不能执行，registry 执行工具，trace 留证。
	runtime := agent.NewRuntime(agent.RuntimeConfig{
		MaxSteps:         cfg.MaxSteps,
		LLMTimeout:       cfg.LLMTimeout,
		ToolTimeout:      cfg.ToolTimeout,
		Model:            setup.model,
		TargetContext:    targetContextForDiagnose(cfg),
		ToolArgOverrides: toolArgOverridesForDiagnose(cfg),
		Memories:         memories,
	}, setup.provider, setup.registry, policy.NewValidator(policy.Config{
		ToolAllowlist: cfg.ToolAllowlist,
		ToolSchemas:   toolSchemasFromRegistry(setup.registry),
	}), traceStore)
	runtime.RestorePlan(existingPlan)

	diagnosis, err := runtime.Run(ctx, cfg.Goal)
	if err != nil {
		state := runstore.State{
			RunID:     runID,
			SessionID: cfg.SessionID,
			Goal:      cfg.Goal,
			Status:    runstore.StatusFailed,
			Plan:      runtime.Plan(),
			Trace:     traceStore.List(),
			Error:     tools.RedactSensitive(err.Error()),
			CreatedAt: createdAt,
			UpdatedAt: time.Now().UTC(),
		}
		return diagnoseResult{State: state, RunDir: cfg.RunDir, SessionDir: cfg.SessionDir, Environment: cfg.Environment, OverwriteSessionMemory: cfg.OverwriteSessionMemory, ReportDir: cfg.ReportDir}, err
	}

	markdown := report.Markdown(report.Input{
		Goal:      cfg.Goal,
		Diagnosis: *diagnosis,
		Plan:      runtime.Plan(),
		Trace:     traceStore.List(),
	})
	state := runstore.State{
		RunID:     runID,
		SessionID: cfg.SessionID,
		Goal:      cfg.Goal,
		Status:    runstore.StatusCompleted,
		Plan:      runtime.Plan(),
		Diagnosis: diagnosis,
		Trace:     traceStore.List(),
		CreatedAt: createdAt,
		UpdatedAt: time.Now().UTC(),
	}
	return diagnoseResult{Markdown: markdown, State: state, RunDir: cfg.RunDir, SessionDir: cfg.SessionDir, Environment: cfg.Environment, OverwriteSessionMemory: cfg.OverwriteSessionMemory, ReportDir: cfg.ReportDir}, nil
}
