package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	runstore "github.com/y2/go-sre-agent/internal/run"
)

type fakeMemoryChat struct {
	calls  atomic.Int32
	answer func(context.Context, llm.ChatRequest) (string, error)
}

func (f *fakeMemoryChat) Chat(ctx context.Context, req llm.ChatRequest) (string, error) {
	f.calls.Add(1)
	return f.answer(ctx, req)
}
func validModelAnswer(ctx context.Context, req llm.ChatRequest) (string, error) {
	if strings.Contains(req.Messages[0].Content, "Extract") {
		for _, id := range []string{"run_a", "run_b"} {
			if strings.Contains(req.Messages[1].Content, `"run_id":"`+id+`"`) {
				return fmt.Sprintf(`{"candidates":[{"text":"Check Redis identity for %s","applicability":"same service and environment","pending_verifications":["verify current endpoint"],"sources":[{"run_id":"%s","step":1,"tool":"redis_ping","call_id":"call_%s"}]}]}`, id, id, id), nil
			}
		}
	}
	return `{"items":[{"topic":"Redis identity","knowledge":"Check the configured Redis instance","applicability":"same service and environment","conflicts":["Endpoint may differ between deployments"],"source_run_ids":["run_a","run_b"]}]}`, nil
}
func modelFixture(t *testing.T, ids ...string) (*Store, string) {
	t.Helper()
	runDir := t.TempDir()
	store := NewStore(t.TempDir()).WithRunDir(runDir)
	for _, id := range ids {
		state := memoryTestRun(id, "go-chat", "local", "suspected")
		if err := runstore.NewStore(runDir).Save(state); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateForRun(state); err != nil {
			t.Fatal(err)
		}
	}
	return store, runDir
}
func opts(runDir string, limit int) ProcessOptions {
	return ProcessOptions{RunDir: runDir, ExtractModel: "extract", ConsolidationModel: "consolidate", Timeout: time.Second, Limit: limit}
}
func TestModelMemoryExtractConsolidateAndRebuild(t *testing.T) {
	store, runDir := modelFixture(t, "run_a", "run_b")
	fake := &fakeMemoryChat{answer: validModelAnswer}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 3))
	if err != nil || stats.Success != 3 || fake.calls.Load() != 3 {
		t.Fatalf("process = %+v, %v, calls %d", stats, err, fake.calls.Load())
	}
	e, err := store.loadExtraction("run_a")
	if err != nil || e.SourceDigest == "" || e.Candidates[0].Sources[0].CallID != "call_run_a" {
		t.Fatalf("extraction = %+v, %v", e, err)
	}
	c, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", "local")
	if !ok || len(c.Inputs) != 2 {
		t.Fatalf("consolidation = %+v, %v", c, ok)
	}
	for _, path := range []string{store.extractionPath("run_a"), store.consolidationPath("go-chat", "local")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %s: %v, %v", path, info, err)
		}
	}
	matches, err := store.Search(Query{Service: "go-chat", Environment: "local", Goal: "Redis", MaxBytes: 20000})
	if err != nil || len(matches) == 0 || !strings.Contains(matches[0].Content, "历史模型候选经验") || !strings.Contains(matches[0].Content, "call_run_") {
		t.Fatalf("search = %+v, %v", matches, err)
	}
	if err := store.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(); err != nil {
		t.Fatal(err)
	}
	stats, err = store.Process(context.Background(), fake, opts(runDir, 3))
	if err != nil || fake.calls.Load() != 3 || stats.Success != 0 {
		t.Fatalf("repeat = %+v, %v, calls %d", stats, err, fake.calls.Load())
	}
}
func mustRollouts(t *testing.T, s *Store) []rolloutDocument {
	t.Helper()
	docs, err := s.loadAllRollouts()
	if err != nil {
		t.Fatal(err)
	}
	return docs
}
func TestInvalidModelOutputsFallBackToDeterministicMemory(t *testing.T) {
	cases := map[string]string{
		"json":        `not JSON`,
		"forged step": `{"candidates":[{"text":"bad","applicability":"local","sources":[{"run_id":"run_a","step":99,"tool":"redis_ping","call_id":"call_run_a"}]}]}`,
		"cross run":   `{"candidates":[{"text":"bad","applicability":"local","sources":[{"run_id":"run_other","step":1,"tool":"redis_ping","call_id":"call_run_a"}]}]}`,
		"upgrade":     `{"candidates":[{"text":"bad","applicability":"local","conclusion_status":"identified","sources":[{"run_id":"run_a","step":1,"tool":"redis_ping","call_id":"call_run_a"}]}]}`,
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			store, runDir := modelFixture(t, "run_a")
			fake := &fakeMemoryChat{answer: func(context.Context, llm.ChatRequest) (string, error) { return response, nil }}
			stats, _ := store.Process(context.Background(), fake, opts(runDir, 1))
			if stats.Failed != 1 {
				t.Fatalf("stats = %+v", stats)
			}
			matches, err := store.Search(Query{Service: "go-chat", Environment: "local"})
			if err != nil || len(matches) != 1 || strings.Contains(matches[0].Content, "历史模型候选经验") {
				t.Fatalf("fallback = %+v, %v", matches, err)
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		store, runDir := modelFixture(t, "run_a")
		fake := &fakeMemoryChat{answer: func(ctx context.Context, _ llm.ChatRequest) (string, error) { <-ctx.Done(); return "", ctx.Err() }}
		o := opts(runDir, 1)
		o.Timeout = time.Millisecond
		stats, _ := store.Process(context.Background(), fake, o)
		if stats.Failed != 1 {
			t.Fatalf("timeout = %+v", stats)
		}
	})
}
func TestModelMemoryLifecycleInvalidatesConsolidation(t *testing.T) {
	store, runDir := modelFixture(t, "run_a", "run_b")
	fake := &fakeMemoryChat{answer: validModelAnswer}
	if stats, err := store.Process(context.Background(), fake, opts(runDir, 3)); err != nil || stats.Success != 3 {
		t.Fatalf("initial = %+v, %v", stats, err)
	}
	if err := store.Correct("run_a", "undetermined", "weaker evidence"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", "local"); ok {
		t.Fatal("corrected consolidation still valid")
	}
	matches, _ := store.Search(Query{Service: "go-chat", Environment: "local", MaxBytes: 20000})
	for _, m := range matches {
		if strings.Contains(m.Content, "整合主题") {
			t.Fatal("stale consolidation injected")
		}
	}
	if err := store.Invalidate("run_b", "no longer applies"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", "local"); ok {
		t.Fatal("invalidated consolidation still valid")
	}
	if err := store.Delete("run_a", "remove"); err != nil {
		t.Fatal(err)
	}
	matches, _ = store.Search(Query{Service: "go-chat", Environment: "local"})
	if len(matches) != 0 {
		t.Fatalf("deleted/invalidated = %+v", matches)
	}
}
func TestModelMemoryRunChangeAndOfflineRebuild(t *testing.T) {
	store, runDir := modelFixture(t, "run_a", "run_b")
	fake := &fakeMemoryChat{answer: validModelAnswer}
	_, _ = store.Process(context.Background(), fake, opts(runDir, 3))
	state, err := runstore.NewStore(runDir).Load("run_a")
	if err != nil {
		t.Fatal(err)
	}
	state.Goal = "changed source"
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", "local"); ok {
		t.Fatal("changed run retained consolidation")
	}
	matches, _ := store.Search(Query{Service: "go-chat", Environment: "local", MaxBytes: 20000})
	for _, m := range matches {
		if m.RunID == "run_a" && strings.Contains(m.Content, "历史模型候选经验") {
			t.Fatal("changed run injected old model text")
		}
	}
	if err := store.Rebuild(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(store.dir, indexFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(store.dir, indexFileName))
	if string(before) != string(after) {
		t.Fatal("offline rebuild not idempotent")
	}
	stale, err := store.Process(context.Background(), &fakeMemoryChat{answer: func(context.Context, llm.ChatRequest) (string, error) { return "", errors.New("unexpected call") }}, opts(runDir, 1))
	if err != nil || stale.Stale != 1 {
		t.Fatalf("stale = %+v, %v", stale, err)
	}
}

func TestConsolidationRejectsCrossEnvironmentRun(t *testing.T) {
	runDir := t.TempDir()
	store := NewStore(t.TempDir()).WithRunDir(runDir)
	for _, scope := range []struct{ id, env string }{{"run_a", "local"}, {"run_b", "staging"}} {
		state := memoryTestRun(scope.id, "go-chat", scope.env, "suspected")
		if err := runstore.NewStore(runDir).Save(state); err != nil {
			t.Fatal(err)
		}
		if _, err := store.UpdateForRun(state); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeMemoryChat{answer: validModelAnswer}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 4))
	if err != nil || stats.Success != 2 || stats.Failed != 2 {
		t.Fatalf("cross-scope result = %+v, %v", stats, err)
	}
	for _, env := range []string{"local", "staging"} {
		if _, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", env); ok {
			t.Fatalf("cross-environment consolidation accepted for %s", env)
		}
	}
}

func TestConcurrentProcessorsUseSingleTaskLease(t *testing.T) {
	store, runDir := modelFixture(t, "run_a")
	var calls atomic.Int32
	fake := &fakeMemoryChat{answer: func(_ context.Context, req llm.ChatRequest) (string, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		if strings.Contains(req.Messages[0].Content, "Extract") {
			return `{"candidates":[{"text":"Verify Redis identity","applicability":"local","sources":[{"run_id":"run_a","step":1,"tool":"redis_ping","call_id":"call_run_a"}]}]}`, nil
		}
		return `{"items":[{"topic":"Redis","knowledge":"Verify identity","applicability":"local","conflicts":[],"source_run_ids":["run_a"]}]}`, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = NewStore(store.dir).WithRunDir(runDir).Process(context.Background(), fake, opts(runDir, 2))
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("duplicate model calls: %d", calls.Load())
	}
	if _, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", "local"); !ok {
		t.Fatal("missing consolidation")
	}
}

func TestEachLifecycleEditDropsOldConsolidation(t *testing.T) {
	edits := map[string]func(*Store) error{
		"correct":    func(s *Store) error { return s.Correct("run_a", "undetermined", "weaker evidence") },
		"invalidate": func(s *Store) error { return s.Invalidate("run_a", "moved instance") },
		"delete":     func(s *Store) error { return s.Delete("run_a", "obsolete") },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			store, runDir := modelFixture(t, "run_a", "run_b")
			fake := &fakeMemoryChat{answer: validModelAnswer}
			if stats, err := store.Process(context.Background(), fake, opts(runDir, 3)); err != nil || stats.Success != 3 {
				t.Fatalf("initial = %+v, %v", stats, err)
			}
			if err := edit(store); err != nil {
				t.Fatal(err)
			}
			if _, ok := store.validConsolidation(mustRollouts(t, store), runDir, "go-chat", "local"); ok {
				t.Fatal("old consolidation is still valid")
			}
			matches, err := store.Search(Query{Service: "go-chat", Environment: "local", MaxBytes: 20000})
			if err != nil {
				t.Fatal(err)
			}
			for _, match := range matches {
				if strings.Contains(match.Content, "整合主题") {
					t.Fatal("old consolidation injected")
				}
			}
		})
	}
}
