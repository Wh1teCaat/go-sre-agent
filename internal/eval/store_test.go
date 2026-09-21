package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreSavesRedactedAtomicResultWithPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	result := testResult("eval_store_test", StatusFailed)
	result.Command = "sre-agent eval model --token=top-secret-token"
	result.Error = "provider failed: api_key=top-secret-key"
	result.Assertions = []Assertion{{
		Name:   "credential_check",
		Passed: false,
		Detail: "password=top-secret-password",
	}}

	path, err := store.Save(result)
	if err != nil {
		t.Fatalf("save result: %v", err)
	}
	if path != filepath.Join(dir, result.ID+".json") {
		t.Fatalf("path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat saved result: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("result mode = %o, want 600", mode)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved result: %v", err)
	}
	for _, secret := range []string{"top-secret-token", "top-secret-key", "top-secret-password"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("saved result leaked %q:\n%s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("saved result does not show redaction:\n%s", raw)
	}

	loaded, err := store.Load(result.ID)
	if err != nil {
		t.Fatalf("load result: %v", err)
	}
	if loaded.Status != StatusFailed || loaded.Mode != ModeMock || loaded.ExecutedRealModel {
		t.Fatalf("loaded result = %#v", loaded)
	}
	if strings.Contains(loaded.Error, "top-secret-key") || strings.Contains(loaded.Command, "top-secret-token") {
		t.Fatalf("loaded result leaked secret: %#v", loaded)
	}
}

func TestStoreAtomicallyReplacesExistingResult(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	first := testResult("eval_replace_test", StatusFailed)
	first.Error = "first failure"
	if _, err := store.Save(first); err != nil {
		t.Fatalf("save first result: %v", err)
	}

	second := testResult("eval_replace_test", StatusPassed)
	second.Assertions = []Assertion{{Name: "final_diagnosis", Passed: true}}
	if _, err := store.Save(second); err != nil {
		t.Fatalf("save replacement result: %v", err)
	}
	loaded, err := store.Load(second.ID)
	if err != nil {
		t.Fatalf("load replacement: %v", err)
	}
	if loaded.Status != StatusPassed || loaded.Error != "" {
		t.Fatalf("loaded replacement = %#v", loaded)
	}
	temporary, err := filepath.Glob(filepath.Join(dir, ".eval-*.tmp"))
	if err != nil {
		t.Fatalf("find temporary files: %v", err)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary files remain: %v", temporary)
	}
}

func TestStorePreservesSkippedRealModelStatus(t *testing.T) {
	store := NewStore(t.TempDir())
	result := testResult("eval_skipped_real", StatusSkipped)
	result.Mode = ModeReal
	result.Model = "configured-model"
	result.Command = "sre-agent eval model --scenario skeleton"
	result.ExecutedRealModel = false
	if _, err := store.Save(result); err != nil {
		t.Fatalf("save skipped result: %v", err)
	}
	loaded, err := store.Load(result.ID)
	if err != nil {
		t.Fatalf("load skipped result: %v", err)
	}
	if loaded.Status != StatusSkipped || loaded.Mode != ModeReal || loaded.ExecutedRealModel {
		t.Fatalf("loaded skipped result = %#v", loaded)
	}
}

func TestStoreRejectsUnsafeResultIDAndInvalidMode(t *testing.T) {
	store := NewStore(t.TempDir())
	unsafe := testResult("../escape", StatusPassed)
	if _, err := store.Save(unsafe); err == nil || !strings.Contains(err.Error(), "unsafe evaluation result id") {
		t.Fatalf("unsafe save error = %v", err)
	}
	if _, err := store.Load("../escape"); err == nil || !strings.Contains(err.Error(), "unsafe evaluation result id") {
		t.Fatalf("unsafe load error = %v", err)
	}

	invalidMode := testResult("eval_invalid_mode", StatusPassed)
	invalidMode.Mode = "unexpected"
	if _, err := store.Save(invalidMode); err == nil || !strings.Contains(err.Error(), "unsupported evaluation mode") {
		t.Fatalf("invalid mode error = %v", err)
	}

	unenforcedRealPass := testResult("eval_real_without_execution", StatusPassed)
	unenforcedRealPass.Mode = ModeReal
	unenforcedRealPass.Model = "configured-model"
	if _, err := store.Save(unenforcedRealPass); err == nil || !strings.Contains(err.Error(), "requires an executed model attempt") {
		t.Fatalf("unexecuted real pass error = %v", err)
	}
}

func TestNewResultIDIsSafeAndUnique(t *testing.T) {
	now := time.Unix(1700000000, 123).UTC()
	first := NewResultID(now)
	second := NewResultID(now)
	if first == second {
		t.Fatalf("result IDs are equal: %q", first)
	}
	if !strings.HasPrefix(first, "eval_20231114_221320_000000123_") {
		t.Fatalf("result ID = %q", first)
	}
	if !safeResultID(first) || !safeResultID(second) {
		t.Fatalf("unsafe result IDs: %q / %q", first, second)
	}
}

func TestNewStoreUsesDefaultResultsDirectory(t *testing.T) {
	store := NewStore("")
	if store.dir != DefaultResultsDir {
		t.Fatalf("default directory = %q, want %q", store.dir, DefaultResultsDir)
	}
}

func testResult(id string, status Status) Result {
	return Result{
		ID:                id,
		ScenarioID:        ScenarioSkeleton,
		ScenarioName:      "fixture",
		ScenarioVersion:   "v1",
		Mode:              ModeMock,
		Status:            status,
		Model:             "mock",
		ExecutedRealModel: false,
		StartedAt:         time.Unix(10, 0).UTC(),
		FinishedAt:        time.Unix(11, 0).UTC(),
		DurationMS:        1000,
	}
}
