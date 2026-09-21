package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/report"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// saveDiagnosisResult 持久化运行状态，并保留诊断错误与保存错误。
// 参数: result 为运行结果，runErr 为诊断错误；返回: 合并后的错误，全部成功时返回 nil。
func saveDiagnosisResult(result diagnoseResult, runErr error) error {
	var persistErr error
	if result.State.RunID != "" {
		if err := runstore.NewStore(result.RunDir).Save(result.State); err != nil {
			persistErr = fmt.Errorf("save run state: %w", err)
		} else if err := updateSessionForRun(result); err != nil {
			persistErr = fmt.Errorf("save session state: %w", err)
		}
	}
	if runErr == nil {
		if persistErr != nil && result.State.RunID != "" {
			return fmt.Errorf("run_id: %s\n%w", result.State.RunID, persistErr)
		}
		return persistErr
	}
	message := tools.RedactSensitive(runErr.Error())
	if result.State.RunID != "" {
		message = fmt.Sprintf("run_id: %s\n%s", result.State.RunID, message)
	}
	if persistErr != nil {
		message = fmt.Sprintf("%s\n%v", message, persistErr)
	}
	return fmt.Errorf("%s", message)
}

// readDiagnosisStatus 读取指定 run 并生成 status 命令的 JSON 输出。
// 参数: opts 指定 run id、配置和目录；返回: JSON 文本或读取编码错误。
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
		SessionID  string          `json:"session_id,omitempty"`
		Goal       string          `json:"goal"`
		Status     runstore.Status `json:"status"`
		Plan       schema.Plan     `json:"plan"`
		Error      string          `json:"error,omitempty"`
		TraceSteps int             `json:"trace_steps"`
		CreatedAt  time.Time       `json:"created_at"`
		UpdatedAt  time.Time       `json:"updated_at"`
	}{
		RunID:      state.RunID,
		SessionID:  state.SessionID,
		Goal:       tools.RedactSensitive(state.Goal),
		Status:     state.Status,
		Plan:       state.Plan,
		Error:      tools.RedactSensitive(state.Error),
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

// renderDiagnosisReport 从已保存的 run 状态重新生成 Markdown 报告。
// 参数: opts 指定 run id、配置和目录；返回: Markdown 报告或读取渲染错误。
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
