package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/report"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
)

func saveDiagnosisRun(result diagnoseResult) error {
	if result.State.RunID == "" {
		return nil
	}
	return runstore.NewStore(result.RunDir).Save(result.State)
}

func readDiagnosisStatus(opts runOptions) (string, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return "", fmt.Errorf("run id is required")
	}
	runDir, err := resolveRunDir(opts.ConfigPath, opts.RunDir)
	if err != nil {
		return "", err
	}
	state, err := runstore.NewStore(runDir).Load(opts.RunID)
	if err != nil {
		return "", err
	}
	view := struct {
		RunID      string          `json:"run_id"`
		Goal       string          `json:"goal"`
		Status     runstore.Status `json:"status"`
		Plan       schema.Plan     `json:"plan"`
		Error      string          `json:"error,omitempty"`
		TraceSteps int             `json:"trace_steps"`
		CreatedAt  time.Time       `json:"created_at"`
		UpdatedAt  time.Time       `json:"updated_at"`
	}{
		RunID:      state.RunID,
		Goal:       state.Goal,
		Status:     state.Status,
		Plan:       state.Plan,
		Error:      state.Error,
		TraceSteps: len(state.Trace),
		CreatedAt:  state.CreatedAt,
		UpdatedAt:  state.UpdatedAt,
	}
	data, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode run status: %w", err)
	}
	return string(data), nil
}

func renderDiagnosisReport(opts runOptions) (string, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return "", fmt.Errorf("run id is required")
	}
	runDir, err := resolveRunDir(opts.ConfigPath, opts.RunDir)
	if err != nil {
		return "", err
	}
	state, err := runstore.NewStore(runDir).Load(opts.RunID)
	if err != nil {
		return "", err
	}
	if state.Diagnosis == nil {
		return "", fmt.Errorf("run %q has no final diagnosis", opts.RunID)
	}
	return report.Markdown(report.Input{
		Goal:      state.Goal,
		Diagnosis: *state.Diagnosis,
		Plan:      state.Plan,
		Trace:     state.Trace,
	}), nil
}
