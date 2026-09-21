package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreSavesRedactedPrivateSessionStateAndMemory(t *testing.T) {
	store := NewStore(t.TempDir())
	state := State{
		SessionID:   "session_test",
		Goal:        `diagnose password="top-secret"`,
		Environment: "local",
		RunIDs:      []string{"run_one"},
		CreatedAt:   time.Unix(10, 0).UTC(),
		UpdatedAt:   time.Unix(20, 0).UTC(),
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := store.SaveMemory(state.SessionID, "token=top-secret-token"); err != nil {
		t.Fatalf("save memory: %v", err)
	}

	statePath, err := store.statePath(state.SessionID)
	if err != nil {
		t.Fatalf("state path: %v", err)
	}
	memoryPath, err := store.memoryPath(state.SessionID)
	if err != nil {
		t.Fatalf("memory path: %v", err)
	}
	for _, path := range []string{statePath, memoryPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mode for %q = %o, want 600", path, got)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %q: %v", path, err)
		}
		if strings.Contains(string(data), "top-secret") {
			t.Fatalf("persisted session data leaked secret in %q: %s", path, data)
		}
	}

	loaded, err := store.Load(state.SessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	if loaded.SessionID != state.SessionID || loaded.RunIDs[0] != "run_one" || !strings.Contains(loaded.Goal, "[REDACTED]") {
		t.Fatalf("loaded state = %#v", loaded)
	}
	memory, err := store.LoadMemory(state.SessionID)
	if err != nil {
		t.Fatalf("load memory: %v", err)
	}
	if strings.Contains(memory, "top-secret") || !strings.Contains(memory, "[REDACTED]") {
		t.Fatalf("loaded memory = %q", memory)
	}
	if got, want := MemoryDigest(memory), MemoryDigest("token=top-secret-token"); got != want {
		t.Fatalf("memory digest = %q, want %q", got, want)
	}
}

func TestStoreAtomicallyReplacesSessionFiles(t *testing.T) {
	store := NewStore(t.TempDir())
	state := testState("session_replace")
	if err := store.Save(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	if err := store.SaveMemory(state.SessionID, "first"); err != nil {
		t.Fatalf("save first memory: %v", err)
	}
	state.RunIDs = append(state.RunIDs, "run_two")
	state.UpdatedAt = time.Unix(30, 0).UTC()
	if err := store.Save(state); err != nil {
		t.Fatalf("replace state: %v", err)
	}
	if err := store.SaveMemory(state.SessionID, "second"); err != nil {
		t.Fatalf("replace memory: %v", err)
	}

	loaded, err := store.Load(state.SessionID)
	if err != nil {
		t.Fatalf("load replacement: %v", err)
	}
	if len(loaded.RunIDs) != 2 || loaded.RunIDs[1] != "run_two" {
		t.Fatalf("loaded replacement = %#v", loaded)
	}
	memory, err := store.LoadMemory(state.SessionID)
	if err != nil || memory != "second" {
		t.Fatalf("replacement memory = %q, %v", memory, err)
	}
	dir, err := store.sessionDir(state.SessionID)
	if err != nil {
		t.Fatalf("session dir: %v", err)
	}
	temporary, err := filepath.Glob(filepath.Join(dir, ".session-*.tmp"))
	if err != nil {
		t.Fatalf("find temporary files: %v", err)
	}
	if len(temporary) != 0 {
		t.Fatalf("temporary files remain: %v", temporary)
	}
}

func TestStoreRejectsUnsafeSessionMetadata(t *testing.T) {
	store := NewStore(t.TempDir())
	unsafe := testState("../escape")
	if err := store.Save(unsafe); err == nil || !strings.Contains(err.Error(), "unsafe session id") {
		t.Fatalf("unsafe save error = %v", err)
	}
	if _, err := store.Load("../escape"); err == nil || !strings.Contains(err.Error(), "unsafe session id") {
		t.Fatalf("unsafe load error = %v", err)
	}
	duplicateRuns := testState("session_duplicate")
	duplicateRuns.RunIDs = []string{"run_one", "run_one"}
	if err := store.Save(duplicateRuns); err == nil || !strings.Contains(err.Error(), "duplicate session run id") {
		t.Fatalf("duplicate run error = %v", err)
	}
}

func TestLoadMemoryAllowsMissingGeneratedFile(t *testing.T) {
	store := NewStore(t.TempDir())
	state := testState("session_missing_memory")
	if err := store.Save(state); err != nil {
		t.Fatalf("save state: %v", err)
	}
	memory, err := store.LoadMemory(state.SessionID)
	if err != nil || memory != "" {
		t.Fatalf("missing memory = %q, %v", memory, err)
	}
}

func TestNewIDIsSafeAndUnique(t *testing.T) {
	now := time.Unix(1700000000, 123).UTC()
	first := NewID(now)
	second := NewID(now)
	if first == second || !safeID(first) || !safeID(second) {
		t.Fatalf("session ids = %q / %q", first, second)
	}
	if !strings.HasPrefix(first, "session_20231114_221320_000000123_") {
		t.Fatalf("session id = %q", first)
	}
}

func testState(sessionID string) State {
	return State{
		SessionID:   sessionID,
		Goal:        "diagnose login",
		Environment: "local",
		RunIDs:      []string{"run_one"},
		CreatedAt:   time.Unix(10, 0).UTC(),
		UpdatedAt:   time.Unix(20, 0).UTC(),
	}
}
