package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestStoreSavesAndLoadsRunState(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	state := State{
		RunID:  "run_test",
		Goal:   "诊断登录 500",
		Status: StatusCompleted,
		Plan: schema.Plan{
			Items: []schema.PlanItem{
				{ID: "login", Goal: "检查登录接口"},
			},
		},
		Diagnosis: &schema.Diagnosis{
			Summary: "登录接口返回 500",
		},
		Trace: []trace.Entry{
			{
				Step:     1,
				ToolName: "http_check",
				Result: schema.Observation{
					Tool:    "http_check",
					Summary: "returned 500",
				},
				Duration: time.Millisecond,
			},
		},
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(20, 0).UTC(),
	}

	if err := store.Save(state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	loaded, err := store.Load("run_test")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if loaded.RunID != "run_test" {
		t.Fatalf("run id = %q, want run_test", loaded.RunID)
	}
	if loaded.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", loaded.Status)
	}
	if loaded.Diagnosis == nil || loaded.Diagnosis.Summary != "登录接口返回 500" {
		t.Fatalf("diagnosis = %#v", loaded.Diagnosis)
	}
	if len(loaded.Trace) != 1 || loaded.Trace[0].ToolName != "http_check" {
		t.Fatalf("trace = %#v", loaded.Trace)
	}
	if len(loaded.Plan.Items) != 1 || loaded.Plan.Items[0].ID != "login" {
		t.Fatalf("plan = %#v", loaded.Plan)
	}
	info, err := os.Stat(filepath.Join(dir, "run_test.json"))
	if err != nil {
		t.Fatalf("stat run state: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("run state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStoreRejectsUnsafeRunID(t *testing.T) {
	store := NewStore(t.TempDir())

	err := store.Save(State{RunID: "../escape", Status: StatusCompleted})
	if err == nil {
		t.Fatal("save succeeded, want unsafe run id error")
	}
	if !strings.Contains(err.Error(), "unsafe run id") {
		t.Fatalf("error = %q, want unsafe run id", err.Error())
	}

	if _, err := store.Load("../escape"); err == nil {
		t.Fatal("load succeeded, want unsafe run id error")
	}
}

func TestNewRunIDUsesRandomSuffix(t *testing.T) {
	now := time.Unix(1700000000, 123).UTC()

	first := NewRunID(now)
	second := NewRunID(now)

	if first == second {
		t.Fatalf("run ids are equal: %q", first)
	}
	if !strings.HasPrefix(first, "run_20231114_221320_000000123_") {
		t.Fatalf("run id = %q, want timestamp prefix plus suffix", first)
	}
	if !safeRunID(first) || !safeRunID(second) {
		t.Fatalf("unsafe run ids: %q %q", first, second)
	}
}

func TestStoreReturnsRecentCompletedRunsForMemory(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	for _, state := range []State{
		{RunID: "old", Status: StatusCompleted, Goal: "旧诊断", Diagnosis: &schema.Diagnosis{Summary: "旧结论"}, UpdatedAt: time.Unix(10, 0)},
		{RunID: "new", Status: StatusCompleted, Goal: "新诊断", Diagnosis: &schema.Diagnosis{Summary: "新结论"}, UpdatedAt: time.Unix(20, 0)},
		{RunID: "failed", Status: StatusFailed, Goal: "失败诊断", UpdatedAt: time.Unix(30, 0)},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("save state: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write broken state: %v", err)
	}

	states := store.RecentCompleted(1)
	if len(states) != 1 || states[0].RunID != "new" {
		t.Fatalf("recent states = %#v, want newest completed run", states)
	}
}
