package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyV1CompletedRunID = "run_legacy_v1"

// TestLegacyV1CompletedRunSupportsStatusAndReport fixes the first persisted
// run-file shape as a compatibility fixture. In particular, v1 trace entries
// do not have PlanItemID and v1 diagnoses do not have root_cause.
func TestLegacyV1CompletedRunSupportsStatusAndReport(t *testing.T) {
	fixturePath := filepath.Join("testdata", "legacy_runs", "v1_completed.json")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read legacy fixture: %v", err)
	}
	for _, field := range []string{`"root_cause"`, `"PlanItemID"`} {
		if strings.Contains(string(fixture), field) {
			t.Fatalf("legacy fixture unexpectedly contains %s", field)
		}
	}

	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, legacyV1CompletedRunID+".json"), fixture, 0o600); err != nil {
		t.Fatalf("copy legacy fixture to run dir: %v", err)
	}

	status, err := readDiagnosisStatus(runOptions{RunID: legacyV1CompletedRunID, RunDir: runDir})
	if err != nil {
		t.Fatalf("read legacy run status: %v", err)
	}
	var statusView struct {
		RunID      string `json:"run_id"`
		Status     string `json:"status"`
		TraceSteps int    `json:"trace_steps"`
	}
	if err := json.Unmarshal([]byte(status), &statusView); err != nil {
		t.Fatalf("decode status output: %v\n%s", err, status)
	}
	if statusView.RunID != legacyV1CompletedRunID || statusView.Status != "completed" || statusView.TraceSteps != 1 {
		t.Fatalf("legacy status = %#v, want run_id=%q status=completed trace_steps=1", statusView, legacyV1CompletedRunID)
	}

	markdown, err := renderDiagnosisReport(runOptions{RunID: legacyV1CompletedRunID, RunDir: runDir})
	if err != nil {
		t.Fatalf("render legacy run report: %v", err)
	}
	for _, want := range []string{
		"诊断脱敏示例服务的登录错误",
		"登录接口返回 500；需继续结合当前证据验证根因。",
		"Step 1 `http_check`",
		"POST https://service.example.invalid/v1/user/login returned 500",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("legacy report missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "模型摘要不应直接作为报告证据") {
		t.Fatalf("legacy report used model evidence instead of trace observation:\n%s", markdown)
	}
	if strings.Contains(markdown, "## Root Cause") {
		t.Fatalf("legacy report unexpectedly inferred a root cause:\n%s", markdown)
	}
}
