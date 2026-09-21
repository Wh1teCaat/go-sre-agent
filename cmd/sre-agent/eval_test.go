package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	evaluation "github.com/y2/go-sre-agent/internal/eval"
)

func TestRunMockEvaluationsRunsOfflineFixturesAndWritesResults(t *testing.T) {
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "not-a-real-provider")
	resultsDir := t.TempDir()

	results, err := runMockEvaluations(context.Background(), "all", resultsDir)
	if err != nil {
		t.Fatalf("run mock evaluations: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("result count = %d, want 2", len(results))
	}
	for _, stored := range results {
		if stored.Result.Status != evaluation.StatusPassed {
			t.Fatalf("scenario %q status = %q, want passed", stored.Result.ScenarioID, stored.Result.Status)
		}
		if stored.Result.Mode != evaluation.ModeMock || stored.Result.ExecutedRealModel {
			t.Fatalf("scenario %q mode/executed = %q/%t, want mock/false", stored.Result.ScenarioID, stored.Result.Mode, stored.Result.ExecutedRealModel)
		}
		if stored.Result.Model != "mock" {
			t.Fatalf("scenario %q model = %q, want mock", stored.Result.ScenarioID, stored.Result.Model)
		}
		info, err := os.Stat(stored.ResultPath)
		if err != nil {
			t.Fatalf("stat persisted result %q: %v", stored.ResultPath, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("result mode = %o, want 600", got)
		}
		loaded, err := evaluation.NewStore(resultsDir).Load(stored.Result.ID)
		if err != nil {
			t.Fatalf("load persisted result: %v", err)
		}
		if loaded.Status != evaluation.StatusPassed || loaded.Command == "" || loaded.ScenarioVersion != "v1" {
			t.Fatalf("persisted result = %#v", loaded)
		}
	}
}

func TestRunModelEvaluationSkipsWithoutRealModelAuthorization(t *testing.T) {
	resultsDir := t.TempDir()
	result, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:   "login-500",
		ResultsDir: resultsDir,
	})
	if err != nil {
		t.Fatalf("run skipped model evaluation: %v", err)
	}
	if result.Result.Status != evaluation.StatusSkipped {
		t.Fatalf("status = %q, want skipped", result.Result.Status)
	}
	if result.Result.ExecutedRealModel {
		t.Fatal("skipped model evaluation recorded a real-model execution")
	}
	if !strings.Contains(result.Result.Error, "not authorized") {
		t.Fatalf("skip reason = %q", result.Result.Error)
	}
	loaded, err := evaluation.NewStore(resultsDir).Load(result.Result.ID)
	if err != nil {
		t.Fatalf("load skipped evaluation result: %v", err)
	}
	if loaded.Status != evaluation.StatusSkipped || loaded.ExecutedRealModel {
		t.Fatalf("persisted skipped result = %#v", loaded)
	}
}

func TestEvaluationReportsFailureWhenResultCannotBePersisted(t *testing.T) {
	resultPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(resultPath, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write results path fixture: %v", err)
	}

	mockResults, err := runMockEvaluations(context.Background(), "skeleton", resultPath)
	if err == nil {
		t.Fatal("mock evaluation succeeded with an unwritable results directory")
	}
	if len(mockResults) != 1 || mockResults[0].Result.Status != evaluation.StatusFailed || mockResults[0].Result.Error != "evaluation result could not be persisted" {
		t.Fatalf("mock persistence failure = %#v", mockResults)
	}

	modelResult, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:   "login-500",
		ResultsDir: resultPath,
	})
	if err == nil {
		t.Fatal("skipped model evaluation succeeded without a result record")
	}
	if modelResult.Result.Status != evaluation.StatusFailed || modelResult.Result.Error != "evaluation result could not be persisted" {
		t.Fatalf("model persistence failure = %#v", modelResult)
	}
}

func TestRunModelEvaluationDoesNotClaimExecutionWhenSetupFails(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "eval-test-model")

	result, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:         "login-500",
		ConfigPath:       filepath.Join(t.TempDir(), "missing-config.yaml"),
		ResultsDir:       t.TempDir(),
		ExecuteRealModel: true,
	})
	if err == nil {
		t.Fatal("model evaluation succeeded with a missing evaluation config")
	}
	if result.Result.Status != evaluation.StatusFailed || result.Result.ExecutedRealModel {
		t.Fatalf("setup failure result = %#v", result.Result)
	}
	if result.Result.Error != "real model evaluation failed before completion; provider details were not persisted" {
		t.Fatalf("setup failure reason = %q", result.Result.Error)
	}
}

func TestRealModelEvaluationFixtureNeverRequestsModelSelectedTarget(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "eval-test-model")

	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	modelCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls++
		var content string
		switch modelCalls {
		case 1:
			content = fmt.Sprintf(`{"type":"tool_call","thought_summary":"try a non-fixture target","tool":"http_check","args":{"url":%q,"method":"POST"}}`, target.URL+"/sensitive")
		case 2:
			content = `{"type":"final","thought_summary":"fixture rejected the target","final":{"summary":"登录接口返回 500。","root_cause":{"status":"undetermined","statement":"fixture failure is not a root cause."},"evidence":[{"step":1,"tool":"http_check","summary":"fixture rejected target"}]}}`
		default:
			t.Fatalf("unexpected model call %d", modelCalls)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		}); err != nil {
			t.Fatalf("write model response: %v", err)
		}
	}))
	defer modelServer.Close()
	t.Setenv("OPENAI_BASE_URL", modelServer.URL)

	result, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:         "login-500",
		ConfigPath:       writeTestConfig(t, "agent:\n  skill_path: ../../skills/sre-diagnosis/SKILL.md\n"),
		ResultsDir:       t.TempDir(),
		ExecuteRealModel: true,
	})
	if err == nil {
		t.Fatal("unsafe-target model evaluation unexpectedly passed")
	}
	if modelCalls != 2 || targetHits.Load() != 0 {
		t.Fatalf("model calls/target hits = %d/%d, want 2/0", modelCalls, targetHits.Load())
	}
	if result.Result.Status != evaluation.StatusFailed || !result.Result.ExecutedRealModel {
		t.Fatalf("unsafe-target result = %#v", result.Result)
	}
}

func TestRejectUnexpectedEvalArgs(t *testing.T) {
	fs := flag.NewFlagSet("eval model", flag.ContinueOnError)
	_ = fs.Bool("execute-real-model", false, "")
	if err := fs.Parse([]string{"--execute-real-model", "false"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if err := rejectUnexpectedEvalArgs(fs); err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
		t.Fatalf("unexpected args error = %v", err)
	}
}

func TestRunModelEvaluationUsesExplicitGateAndLocalModelFixture(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "eval-test-model")

	modelCalls := 0
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("model path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("model authorization = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read model request: %v", err)
		}
		var request openAIChatCompletionRequestForTest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("decode model request: %v\\n%s", err, body)
		}
		if len(request.Messages) != 2 {
			t.Fatalf("model message count = %d, want 2", len(request.Messages))
		}
		var runtimeContext struct {
			TargetContext struct {
				AllowedPostURLs []string `json:"allowed_post_urls"`
				LogFile         string   `json:"log_file"`
			} `json:"target_context"`
		}
		if err := json.Unmarshal([]byte(request.Messages[1].Content), &runtimeContext); err != nil {
			t.Fatalf("decode runtime context: %v\\n%s", err, request.Messages[1].Content)
		}
		if len(runtimeContext.TargetContext.AllowedPostURLs) != 1 || runtimeContext.TargetContext.LogFile == "" {
			t.Fatalf("fixture target context = %#v", runtimeContext.TargetContext)
		}

		modelCalls++
		var content string
		switch modelCalls {
		case 1:
			content = fmt.Sprintf(`{"type":"tool_call","thought_summary":"reproduce the login failure","tool":"http_check","args":{"url":%q,"method":"POST"}}`, runtimeContext.TargetContext.AllowedPostURLs[0])
		case 2:
			content = fmt.Sprintf(`{"type":"tool_call","thought_summary":"read matching logs","tool":"log_read","args":{"path":%q,"lines":50,"keyword":"ERROR"}}`, runtimeContext.TargetContext.LogFile)
		case 3:
			content = `{"type":"final","thought_summary":"fixture evidence is enough","final":{"summary":"登录接口返回 500，已从本地 fixture 收集 HTTP 和日志证据。","root_cause":{"status":"undetermined","statement":"本次评测不将 fixture 现象提升为根因。"},"evidence":[{"step":1,"tool":"http_check","summary":"HTTP 500"},{"step":2,"tool":"log_read","summary":"ERROR log"}]}}`
		default:
			t.Fatalf("unexpected model call %d", modelCalls)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": content}}},
		}); err != nil {
			t.Fatalf("write model response: %v", err)
		}
	}))
	defer modelServer.Close()
	t.Setenv("OPENAI_BASE_URL", modelServer.URL)

	resultsDir := t.TempDir()
	configPath := writeTestConfig(t, "agent:\n  skill_path: ../../skills/sre-diagnosis/SKILL.md\n")
	result, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:         "login-500",
		ConfigPath:       configPath,
		ResultsDir:       resultsDir,
		ExecuteRealModel: true,
	})
	if err != nil {
		t.Fatalf("run explicit model evaluation: %v", err)
	}
	if modelCalls != 3 {
		t.Fatalf("model calls = %d, want 3", modelCalls)
	}
	if result.Result.Status != evaluation.StatusPassed || !result.Result.ExecutedRealModel {
		t.Fatalf("model evaluation result = %#v", result.Result)
	}
	if result.Result.Model != "eval-test-model" || result.Result.TraceSteps != 3 {
		t.Fatalf("model/trace = %q/%d, want eval-test-model/3", result.Result.Model, result.Result.TraceSteps)
	}
	if !evaluation.AllPassed(result.Result.Assertions) {
		t.Fatalf("model assertions = %#v", result.Result.Assertions)
	}
	loaded, err := evaluation.NewStore(resultsDir).Load(result.Result.ID)
	if err != nil {
		t.Fatalf("load explicit model result: %v", err)
	}
	if loaded.Status != evaluation.StatusPassed || !loaded.ExecutedRealModel || loaded.Command != "eval model --scenario login-500 --execute-real-model" {
		t.Fatalf("persisted explicit model result = %#v", loaded)
	}
}
