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
				Step:       1,
				PlanItemID: "login",
				ToolName:   "http_check",
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
	if loaded.Trace[0].PlanItemID != "login" {
		t.Fatalf("trace plan item = %q, want login", loaded.Trace[0].PlanItemID)
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

func TestStorePreservesCheckpointedCallsAndLegacyOmission(t *testing.T) {
	dir := t.TempDir()
	finishedAt := time.Unix(11, 0).UTC()
	taskDeadline := time.Unix(20, 0).UTC()
	state := State{
		RunID:  "run_checkpointed",
		Goal:   "check durable call state",
		Status: StatusCancelled,
		Calls: []Call{{
			CallID:     "call_test",
			Kind:       CallKindTool,
			Step:       1,
			ToolName:   "smoke_run",
			SideEffect: true,
			Status:     CallStatusUnknown,
			ErrorClass: ErrorClassUnknown,
			Error:      "execution outcome is unknown",
			StartedAt:  time.Unix(10, 0).UTC(),
			FinishedAt: &finishedAt,
		}},
		TaskDeadline: &taskDeadline,
		CreatedAt:    time.Unix(1, 0).UTC(),
		UpdatedAt:    time.Unix(2, 0).UTC(),
	}
	if err := NewStore(dir).Save(state); err != nil {
		t.Fatalf("save checkpointed state: %v", err)
	}
	loaded, err := NewStore(dir).Load(state.RunID)
	if err != nil {
		t.Fatalf("load checkpointed state: %v", err)
	}
	if len(loaded.Calls) != 1 || loaded.Calls[0].CallID != "call_test" || !HasUnknownSideEffect(loaded.Calls) || loaded.TaskDeadline == nil || !loaded.TaskDeadline.Equal(*state.TaskDeadline) {
		t.Fatalf("loaded checkpoint fields = %#v", loaded)
	}

	legacy := State{RunID: "run_legacy", Goal: "old state", Status: StatusFailed, CreatedAt: time.Unix(1, 0).UTC()}
	if err := NewStore(dir).Save(legacy); err != nil {
		t.Fatalf("save legacy-shaped state: %v", err)
	}
	loadedLegacy, err := NewStore(dir).Load(legacy.RunID)
	if err != nil {
		t.Fatalf("load legacy-shaped state: %v", err)
	}
	if len(loadedLegacy.Calls) != 0 || loadedLegacy.TaskDeadline != nil {
		t.Fatalf("legacy checkpoint fields = %#v, want omitted", loadedLegacy)
	}
	legacyJSON, err := os.ReadFile(filepath.Join(dir, legacy.RunID+".json"))
	if err != nil {
		t.Fatalf("read legacy-shaped JSON: %v", err)
	}
	if strings.Contains(string(legacyJSON), `"calls"`) || strings.Contains(string(legacyJSON), `"task_deadline"`) {
		t.Fatalf("legacy-shaped JSON unexpectedly has phase-2 fields:\n%s", legacyJSON)
	}
}

func TestMarkInterruptedCallsUnknownPreservesKnownResults(t *testing.T) {
	calls := []Call{
		{CallID: "call_done", Kind: CallKindTool, Status: CallStatusSucceeded},
		{CallID: "call_running", Kind: CallKindTool, SideEffect: true, Status: CallStatusRunning},
	}
	changed := MarkInterruptedCallsUnknown(calls, time.Unix(10, 0).UTC())
	if !changed || calls[0].Status != CallStatusSucceeded || calls[1].Status != CallStatusUnknown || calls[1].ErrorClass != ErrorClassUnknown || calls[1].FinishedAt == nil || calls[1].FinishedAt.IsZero() {
		t.Fatalf("recovered calls = %#v", calls)
	}
}

func TestStoreOmitsFinishedAtForInFlightCall(t *testing.T) {
	dir := t.TempDir()
	state := State{
		RunID:  "run_in_flight",
		Goal:   "checkpoint before call",
		Status: StatusRunning,
		Calls: []Call{{
			CallID:    "call_running",
			Kind:      CallKindTool,
			Status:    CallStatusRunning,
			StartedAt: time.Unix(10, 0).UTC(),
		}},
		CreatedAt: time.Unix(1, 0).UTC(),
		UpdatedAt: time.Unix(2, 0).UTC(),
	}
	if err := NewStore(dir).Save(state); err != nil {
		t.Fatalf("save in-flight state: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, state.RunID+".json"))
	if err != nil {
		t.Fatalf("read in-flight state: %v", err)
	}
	if strings.Contains(string(data), `"finished_at"`) || strings.Contains(string(data), "0001-01-01") {
		t.Fatalf("in-flight JSON should omit unfinished timestamp:\n%s", data)
	}
}

// TestStoreListReturnsNewestRunsFirst 验证交互恢复列表按最近持久化时间稳定排序，并忽略无关文件。
func TestStoreListReturnsNewestRunsFirst(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	for _, state := range []State{
		{RunID: "run_old", Goal: "old", Status: StatusFailed, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC()},
		{RunID: "run_new", Goal: "new", Status: StatusCancelled, CreatedAt: time.Unix(3, 0).UTC(), UpdatedAt: time.Unix(4, 0).UTC()},
	} {
		if err := store.Save(state); err != nil {
			t.Fatalf("save %s: %v", state.RunID, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
	runs, err := store.List()
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 || runs[0].RunID != "run_new" || runs[1].RunID != "run_old" {
		t.Fatalf("listed runs = %#v", runs)
	}
	if empty, err := NewStore(filepath.Join(dir, "not-created")).List(); err != nil || len(empty) != 0 {
		t.Fatalf("missing directory list = %#v / %v", empty, err)
	}
}
