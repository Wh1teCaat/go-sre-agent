package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
)

const legacyV1FailedRunID = "run_legacy_v1_failed"

// TestLegacyV1FailedRunCanResumeWithMockSkeleton verifies that a failed run
// written before diagnosis.root_cause and trace.PlanItemID existed can resume.
func TestLegacyV1FailedRunCanResumeWithMockSkeleton(t *testing.T) {
	fixturePath := filepath.Join("testdata", "legacy_runs", "v1_failed.json")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read legacy fixture: %v", err)
	}

	runDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runDir, legacyV1FailedRunID+".json"), fixture, 0o600); err != nil {
		t.Fatalf("copy legacy fixture to run dir: %v", err)
	}

	result, err := resumeDiagnosisRun(context.Background(), resumeOptions{
		RunID:      legacyV1FailedRunID,
		RunDir:     runDir,
		ConfigPath: writeTestConfig(t, ""),
		MaxSteps:   1,
	}, "skeleton")
	if err != nil {
		t.Fatalf("resume legacy failed run: %v", err)
	}
	if result.State.RunID != legacyV1FailedRunID {
		t.Fatalf("resumed run id = %q, want %q", result.State.RunID, legacyV1FailedRunID)
	}
	if result.State.Status != runstore.StatusCompleted {
		t.Fatalf("resumed status = %q, want completed", result.State.Status)
	}
	if result.State.Diagnosis == nil || result.State.Diagnosis.Summary != "MVP 诊断闭环验证完成。CLI 已成功运行 runtime 并生成诊断报告。" {
		t.Fatalf("resumed diagnosis = %#v", result.State.Diagnosis)
	}
	if got, want := result.State.CreatedAt, time.Date(2026, time.July, 12, 4, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("resumed created_at = %s, want %s", got, want)
	}
	if len(result.State.Trace) != 1 {
		t.Fatalf("resumed trace entries = %d, want final action only", len(result.State.Trace))
	}
}
