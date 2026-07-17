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

func startDiagnosisRun(ctx context.Context, opts diagnoseOptions, mockScenario string) (diagnoseResult, error) {
	startedAt := time.Now().UTC()
	return executeDiagnosisRun(ctx, opts, mockScenario, runstore.NewRunID(startedAt), startedAt, nil, schema.Plan{})
}

// resumeDiagnosisRun 从已保存的 run state 恢复 trace，并用同一个 run id 更新保存结果。
// 目标和历史 trace 来自 run state；目标环境参数来自配置文件。
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
	if previous.Status == runstore.StatusCompleted && previous.Diagnosis != nil {
		return diagnoseResult{}, fmt.Errorf("run %q is already completed; use report instead", opts.RunID)
	}

	result, runErr := executeDiagnosisRun(ctx, diagnoseOptions{
		Goal:        previous.Goal,
		ConfigPath:  opts.ConfigPath,
		MaxSteps:    opts.MaxSteps,
		LLMTimeout:  opts.LLMTimeout,
		ToolTimeout: opts.ToolTimeout,
		RunDir:      runDir,
	}, mockScenario, previous.RunID, previous.CreatedAt, previous.Trace, previous.Plan)
	return result, runErr
}

func executeDiagnosisRun(ctx context.Context, opts diagnoseOptions, mockScenario string, runID string, createdAt time.Time, existingTrace []trace.Entry, existingPlan schema.Plan) (diagnoseResult, error) {
	setup, err := initializeDiagnosis(opts, mockScenario)
	if err != nil {
		return diagnoseResult{RunDir: setup.config.RunDir, ReportDir: setup.config.ReportDir}, err
	}
	cfg := setup.config
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
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
		Memories:         memoryHintsForDiagnose(cfg.RunDir, runID),
	}, setup.provider, setup.registry, policy.NewValidator(policy.Config{
		ToolAllowlist: cfg.ToolAllowlist,
		ToolSchemas:   toolSchemasFromRegistry(setup.registry),
	}), traceStore)
	runtime.RestorePlan(existingPlan)

	diagnosis, err := runtime.Run(ctx, cfg.Goal)
	if err != nil {
		state := runstore.State{
			RunID:     runID,
			Goal:      cfg.Goal,
			Status:    runstore.StatusFailed,
			Plan:      runtime.Plan(),
			Trace:     traceStore.List(),
			Error:     err.Error(),
			CreatedAt: createdAt,
			UpdatedAt: time.Now().UTC(),
		}
		return diagnoseResult{State: state, RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}

	markdown := report.Markdown(report.Input{
		Goal:      cfg.Goal,
		Diagnosis: *diagnosis,
		Plan:      runtime.Plan(),
		Trace:     traceStore.List(),
	})
	state := runstore.State{
		RunID:     runID,
		Goal:      cfg.Goal,
		Status:    runstore.StatusCompleted,
		Plan:      runtime.Plan(),
		Diagnosis: diagnosis,
		Trace:     traceStore.List(),
		CreatedAt: createdAt,
		UpdatedAt: time.Now().UTC(),
	}
	return diagnoseResult{Markdown: markdown, State: state, RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, nil
}

// memoryHintsForDiagnose 把历史完成运行压缩成模型可见线索。
// 旧 trace/evidence 不进入当前上下文，避免历史步骤被误当成本次证据。
func memoryHintsForDiagnose(runDir string, currentRunID string) []schema.Memory {
	states := runstore.NewStore(runDir).RecentCompleted(3)
	memories := make([]schema.Memory, 0, len(states))
	for _, state := range states {
		if state.RunID == currentRunID {
			continue
		}
		memories = append(memories, schema.Memory{
			Subject:     tools.RedactSensitive(state.Goal),
			Content:     tools.RedactSensitive(state.Diagnosis.Summary),
			SourceRunID: state.RunID,
		})
	}
	return memories
}
