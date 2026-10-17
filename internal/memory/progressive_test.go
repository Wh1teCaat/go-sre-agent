package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestExtractionVisitsLateFactsAndPreservesDetail(t *testing.T) {
	store, runDir := modelFixture(t, "run_a")
	state, err := runstore.NewStore(runDir).Load("run_a")
	if err != nil {
		t.Fatal(err)
	}
	state.Trace = nil
	for step := 1; step <= 24; step++ {
		state.Trace = append(state.Trace, trace.Entry{Step: step, ToolName: "inspect_tls", CallID: fmt.Sprintf("call_%d", step), Result: schema.Observation{
			Tool: "inspect_tls", Summary: strings.Repeat("observation detail ", 240), Target: schema.TargetIdentity{Kind: "service", ID: "edge"}, CheckStatus: "completed", TargetHealth: "unknown",
			Facts: []schema.Fact{{Key: "certificate_serial", Value: fmt.Sprintf("serial-%d", step)}, {Key: "api_token", Value: "do-not-send"}}, Data: map[string]any{"password": "also-secret"},
		}})
	}
	state.Diagnosis.CounterEvidence = []schema.Evidence{{Step: 24, Tool: "inspect_tls", Summary: "late counter-evidence"}}
	state.Diagnosis.PendingVerifications = []schema.PendingVerification{{Question: "check certificate rotation"}}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	fake := &fakeMemoryChat{answer: func(_ context.Context, req llm.ChatRequest) (string, error) {
		input := req.Messages[1].Content
		if strings.Contains(input, "do-not-send") || strings.Contains(input, "also-secret") {
			t.Fatal("unredacted structured data")
		}
		if !strings.Contains(input, "late counter-evidence") || !strings.Contains(input, "check certificate rotation") {
			t.Fatal("lost diagnosis detail")
		}
		var data struct {
			Observations []struct {
				Step   int    `json:"step"`
				Tool   string `json:"tool"`
				CallID string `json:"call_id"`
			} `json:"observations"`
		}
		if err := json.Unmarshal([]byte(input), &data); err != nil {
			t.Fatal(err)
		}
		for _, ob := range data.Observations {
			seen[ob.Step] = true
			if !strings.Contains(input, fmt.Sprintf("serial-%d", ob.Step)) {
				t.Fatal("lost fact")
			}
		}
		first := data.Observations[0]
		result := extractResponse{RolloutSummary: "Detailed source review\n\n" + strings.Repeat("preserve explanation ", 100), Candidates: []Candidate{{Text: strings.Repeat("specific diagnostic detail ", 100) + "TAIL-DETAIL", Keywords: []string{"certificate rotation", "x509"}, Applicability: "same endpoint after rotation", PendingVerifications: []string{"compare issuer fingerprints"}, Sources: []SourceRef{{RunID: "run_a", Step: first.Step, Tool: first.Tool, CallID: first.CallID}}}}}
		encoded, _ := json.Marshal(result)
		return string(encoded), nil
	}}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 1))
	if err != nil || stats.Success != 1 || len(seen) != 24 || fake.calls.Load() < 2 {
		t.Fatalf("chunk extraction: %+v %v seen=%d calls=%d", stats, err, len(seen), fake.calls.Load())
	}
	raw, err := os.ReadFile(filepath.Join(store.dir, rawFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "TAIL-DETAIL") || !strings.Contains(string(raw), "compare issuer fingerprints") {
		t.Fatal("raw memory lost detail")
	}
	doc, err := store.loadRollout("run_a")
	if err != nil || !strings.Contains(doc.ModelSummary, "Detailed source review") {
		t.Fatalf("model rollout: %+v %v", doc, err)
	}
	before, err := store.loadExtraction("run_a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatal(err)
	}
	doc, _ = store.loadRollout("run_a")
	if _, ok := store.validExtraction(doc, runDir); !ok {
		t.Fatal("identical collection invalidated model review")
	}
	after, _ := store.loadExtraction("run_a")
	if after.ContentDigest != before.ContentDigest {
		t.Fatal("changed extraction on repeated collection")
	}
}

func TestEmptyExperienceIsRememberedAsSkipped(t *testing.T) {
	store, runDir := modelFixture(t, "run_a")
	fake := &fakeMemoryChat{answer: func(_ context.Context, req llm.ChatRequest) (string, error) {
		if strings.Contains(req.Messages[0].Content, "Extract") {
			return `{"rollout_summary":"Routine check with no new lesson.","skipped_reason":"No reusable information","candidates":[]}`, nil
		}
		if !strings.Contains(req.Messages[1].Content, "No reusable information") {
			t.Fatal("consolidation did not receive raw memory")
		}
		return `{"items":[]}`, nil
	}}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 2))
	if err != nil || stats.Success != 2 {
		t.Fatalf("process: %+v %v", stats, err)
	}
	if matches, err := store.Search(Query{Service: "go-chat", Environment: "local"}); err != nil || len(matches) != 0 {
		t.Fatalf("skipped lesson is indexed: %+v %v", matches, err)
	}
	stats, err = store.Process(context.Background(), fake, opts(runDir, 2))
	if err != nil || stats.Success != 0 || fake.calls.Load() != 2 {
		t.Fatalf("skip retried: %+v %v", stats, err)
	}
}

func richFixture(t *testing.T) (*Store, string) {
	t.Helper()
	store, runDir := modelFixture(t, "run_a")
	fake := &fakeMemoryChat{answer: func(_ context.Context, req llm.ChatRequest) (string, error) {
		if strings.Contains(req.Messages[0].Content, "Extract") {
			return `{"rollout_summary":"SOURCE-DETAIL: compare certificate serial and issuer after reload.","candidates":[{"text":"RAW-LESSON: a stale worker can keep the old certificate after a reload.","keywords":["x509","certificate rotation"],"applicability":"after certificate replacement","pending_verifications":["check current issuer"],"sources":[{"run_id":"run_a","step":1,"tool":"redis_ping","call_id":"call_run_a"}]}]}`, nil
		}
		if !strings.Contains(req.Messages[1].Content, "RAW-LESSON") || !strings.Contains(req.Messages[1].Content, "check current issuer") {
			t.Fatal("raw memory missing from consolidation")
		}
		return `{"items":[{"topic":"证书轮换后握手失败","summary":"客户端在证书更新后拒绝连接时查阅。","keywords":["x509","certificate rotation","握手失败"],"knowledge":"HANDBOOK-DETAIL: compare the worker's loaded certificate serial with the deployed file.\n\nRestart only after confirming a stale worker.","applicability":"only after certificate rotation","conflicts":["other deployments may have an issuer mismatch"],"source_run_ids":["run_a"]}]}`, nil
	}}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 2))
	if err != nil || stats.Success != 2 {
		t.Fatalf("rich fixture: %+v %v", stats, err)
	}
	return store, runDir
}

func TestProgressiveRetrievalUsesNavigationThenHandbook(t *testing.T) {
	for _, detail := range []bool{false, true} {
		t.Run(fmt.Sprint(detail), func(t *testing.T) {
			store, _ := richFixture(t)
			summary, err := os.ReadFile(filepath.Join(store.dir, summaryFileName))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(summary), "HANDBOOK-DETAIL") || strings.Contains(string(summary), "SOURCE-DETAIL") {
				t.Fatal("navigation duplicates source or handbook")
			}
			stage := 0
			selector := &fakeMemoryChat{answer: func(_ context.Context, req llm.ChatRequest) (string, error) {
				stage++
				input := req.Messages[1].Content
				if stage == 1 {
					if !strings.Contains(input, "客户端在证书更新后") || strings.Contains(input, "HANDBOOK-DETAIL") || strings.Contains(input, "SOURCE-DETAIL") {
						t.Fatalf("not navigation-only: %s", input)
					}
					// The task uses a synonym absent from the old fixed keyword table.
					return `{"topics":[0]}`, nil
				}
				if !strings.Contains(input, "HANDBOOK-DETAIL") || strings.Contains(input, "SOURCE-DETAIL") {
					t.Fatal("second stage should disclose selected handbook only")
				}
				return fmt.Sprintf(`{"runs":[{"run_id":"run_a","need_rollout":%t}]}`, detail), nil
			}}
			hints, err := store.HintsWithModel(context.Background(), Query{Service: "go-chat", Environment: "local", Goal: "更新凭据之后客户端连不上", MaxBytes: 20000}, ReadOptions{Client: selector, Model: "select"})
			if err != nil || len(hints) != 1 || stage != 2 {
				t.Fatalf("hints: %+v %v stage %d", hints, err, stage)
			}
			if !strings.Contains(hints[0].Content, "HANDBOOK-DETAIL") || strings.Contains(hints[0].Content, "SOURCE-DETAIL") != detail || !strings.Contains(hints[0].Content, "issuer mismatch") {
				t.Fatalf("wrong disclosure: %s", hints[0].Content)
			}
		})
	}
}

func TestProgressiveRetrievalRejectsUnscopedSelectionAndHonorsNoMatch(t *testing.T) {
	store, _ := richFixture(t)
	for _, test := range []struct {
		name, answer string
		count        int
	}{{"no match", `{"topics":[]}`, 0}, {"invalid selection", `{"topics":[999]}`, 1}} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMemoryChat{answer: func(context.Context, llm.ChatRequest) (string, error) { return test.answer, nil }}
			hints, err := store.HintsWithModel(context.Background(), Query{Service: "go-chat", Environment: "local"}, ReadOptions{Client: fake, Model: "select"})
			if err != nil || len(hints) != test.count {
				t.Fatalf("hints: %+v %v", hints, err)
			}
		})
	}
	fake := &fakeMemoryChat{answer: func(context.Context, llm.ChatRequest) (string, error) {
		t.Fatal("cross-environment navigation exposed")
		return "", nil
	}}
	hints, err := store.HintsWithModel(context.Background(), Query{Service: "go-chat", Environment: "production"}, ReadOptions{Client: fake, Model: "select"})
	if err != nil || len(hints) != 0 {
		t.Fatalf("scope: %+v %v", hints, err)
	}
}

func TestRetrievalSourceChangeWhileModelRunsDoesNotInjectStaleKnowledge(t *testing.T) {
	store, runDir := richFixture(t)
	stage := 0
	fake := &fakeMemoryChat{answer: func(context.Context, llm.ChatRequest) (string, error) {
		stage++
		if stage == 1 {
			return `{"topics":[0]}`, nil
		}
		state, _ := runstore.NewStore(runDir).Load("run_a")
		state.Goal = "new source"
		if err := runstore.NewStore(runDir).Save(state); err != nil {
			t.Fatal(err)
		}
		return `{"runs":[{"run_id":"run_a","need_rollout":true}]}`, nil
	}}
	hints, err := store.HintsWithModel(context.Background(), Query{Service: "go-chat", Environment: "local"}, ReadOptions{Client: fake, Model: "select"})
	if err != nil {
		t.Fatal(err)
	}
	for _, hint := range hints {
		if strings.Contains(hint.Content, "HANDBOOK-DETAIL") || strings.Contains(hint.Content, "SOURCE-DETAIL") {
			t.Fatal("stale model text injected")
		}
	}
}

func TestExtractionFailedChunkDoesNotCommitPartialMemory(t *testing.T) {
	store, runDir := modelFixture(t, "run_a")
	state, _ := runstore.NewStore(runDir).Load("run_a")
	first := state.Trace[0]
	first.Result.Summary = strings.Repeat("detail ", 7500)
	second := first
	second.Step = 2
	second.CallID = "second"
	state.Trace = []trace.Entry{first, second}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fake := &fakeMemoryChat{answer: func(ctx context.Context, req llm.ChatRequest) (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("second chunk failed")
		}
		return validModelAnswer(ctx, req)
	}}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 1))
	if err != nil || stats.Failed != 1 || calls != 2 {
		t.Fatalf("failed chunk: %+v %v calls %d", stats, err, calls)
	}
	if _, err := os.Stat(store.extractionPath("run_a")); !os.IsNotExist(err) {
		t.Fatalf("partial extraction saved: %v", err)
	}
}

func TestLegacyModelMemoryIsRegeneratedWithoutDiscardingSources(t *testing.T) {
	store, runDir := richFixture(t)
	e, err := store.loadExtraction("run_a")
	if err != nil {
		t.Fatal(err)
	}
	e.Generation = "go-sre-agent/model-memory-v1"
	e.ContentDigest = e.checksum()
	data, _ := json.MarshalIndent(e, "", "  ")
	if err := os.WriteFile(store.extractionPath("run_a"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(); err != nil {
		t.Fatal(err)
	}
	matches, err := store.Search(Query{Service: "go-chat", Environment: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		if strings.Contains(match.Content, "HANDBOOK-DETAIL") {
			t.Fatal("legacy model contents still injected")
		}
	}
	fake := &fakeMemoryChat{answer: validModelAnswer}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 1))
	if err != nil || stats.Success != 1 {
		t.Fatalf("migration: %+v %v", stats, err)
	}
	e, err = store.loadExtraction("run_a")
	if err != nil || e.Generation != modelGeneration {
		t.Fatalf("new extraction: %+v %v", e, err)
	}
	if _, err := runstore.NewStore(runDir).Load("run_a"); err != nil {
		t.Fatal("migration lost source")
	}
}

func TestCorrectionRemovesDetailedModelReview(t *testing.T) {
	store, _ := richFixture(t)
	if err := store.Correct("run_a", "undetermined", "Check new issuer before reaching a conclusion"); err != nil {
		t.Fatal(err)
	}
	doc, err := store.loadRollout("run_a")
	if err != nil {
		t.Fatal(err)
	}
	if doc.ModelSummary != "" {
		t.Fatal("correction retained stale model review")
	}
	raw, err := os.ReadFile(filepath.Join(store.dir, rawFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SOURCE-DETAIL") || strings.Contains(string(raw), "RAW-LESSON") {
		t.Fatal("correction retained stale raw experience")
	}
}

func TestProgressiveRetrievalTimeoutAndCancellation(t *testing.T) {
	store, _ := richFixture(t)
	fake := &fakeMemoryChat{answer: func(ctx context.Context, _ llm.ChatRequest) (string, error) { <-ctx.Done(); return "", ctx.Err() }}
	hints, err := store.HintsWithModel(context.Background(), Query{Service: "go-chat", Environment: "local"}, ReadOptions{Client: fake, Model: "select", Timeout: time.Millisecond})
	if err != nil || len(hints) != 1 {
		t.Fatalf("timeout did not fall back: %+v %v", hints, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.HintsWithModel(ctx, Query{Service: "go-chat", Environment: "local"}, ReadOptions{Client: fake, Model: "select"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
}

func TestMissingConsolidationItemsDoesNotEraseFallback(t *testing.T) {
	store, runDir := modelFixture(t, "run_a")
	fake := &fakeMemoryChat{answer: func(ctx context.Context, req llm.ChatRequest) (string, error) {
		if strings.Contains(req.Messages[0].Content, "Extract") {
			return validModelAnswer(ctx, req)
		}
		return `{}`, nil
	}}
	stats, err := store.Process(context.Background(), fake, opts(runDir, 2))
	if err != nil || stats.Success != 1 || stats.Failed != 1 {
		t.Fatalf("missing items: %+v %v", stats, err)
	}
	if matches, err := store.Search(Query{Service: "go-chat", Environment: "local"}); err != nil || len(matches) != 1 {
		t.Fatalf("fallback lost: %+v %v", matches, err)
	}
}
