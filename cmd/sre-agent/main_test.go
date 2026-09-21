package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func diagnoseOnce(ctx context.Context, opts diagnoseOptions, mockScenario string) (string, error) {
	result, err := startDiagnosisRun(ctx, opts, mockScenario)
	return result.Markdown, err
}

func TestRunDiagnoseLogin500ScenarioExecutesHTTPAndLogTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/user/login" {
			t.Fatalf("path = %q, want /v1/user/login", r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("login failed"))
	}))
	defer server.Close()

	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "chat_proj.log")
	if err := os.WriteFile(logFile, []byte("INFO start\nERROR login failed: pq: relation users does not exist\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	configPath := writeTestConfig(t, fmt.Sprintf(`
targets:
  backend_base_url: %q
  log_file: %q
`, server.URL, logFile))

	markdown, err := diagnoseOnce(context.Background(), diagnoseOptions{
		Goal:        "诊断登录 500",
		ConfigPath:  configPath,
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}, "login-500")
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	for _, want := range []string{
		"登录接口返回 500",
		"Step 1 `http_check`",
		"Step 2 `log_read`",
		`read 1 log lines matching "ERROR"`,
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestSaveDiagnosisRunPersistsCompletedRunState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("login failed"))
	}))
	defer server.Close()

	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "chat_proj.log")
	if err := os.WriteFile(logFile, []byte("ERROR login failed\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	runDir := t.TempDir()
	configPath := writeTestConfig(t, fmt.Sprintf(`
targets:
  backend_base_url: %q
  log_file: %q
`, server.URL, logFile))

	result, err := startDiagnosisRun(context.Background(), diagnoseOptions{
		Goal:        "诊断登录 500",
		ConfigPath:  configPath,
		MaxSteps:    3,
		ToolTimeout: time.Second,
		RunDir:      runDir,
	}, "login-500")
	if err != nil {
		t.Fatalf("start diagnosis: %v", err)
	}
	if err := saveDiagnosisResult(result, nil); err != nil {
		t.Fatalf("save diagnosis: %v", err)
	}
	if result.State.RunID == "" {
		t.Fatal("run id is empty")
	}
	if result.State.Status != runstore.StatusCompleted {
		t.Fatalf("status = %q, want completed", result.State.Status)
	}

	loaded, err := runstore.NewStore(runDir).Load(result.State.RunID)
	if err != nil {
		t.Fatalf("load saved run: %v", err)
	}
	if loaded.Goal != "诊断登录 500" {
		t.Fatalf("goal = %q", loaded.Goal)
	}
	if loaded.Diagnosis == nil || !strings.Contains(loaded.Diagnosis.Summary, "登录接口返回 500") {
		t.Fatalf("diagnosis = %#v", loaded.Diagnosis)
	}
	if len(loaded.Trace) != 3 || loaded.Trace[2].ActionType != schema.ActionTypeFinal {
		t.Fatalf("trace entries = %#v, want two tools and final", loaded.Trace)
	}
}

func TestSaveDiagnosisRunUsesConfiguredRunDir(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "configured-runs")
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
paths:
  run_dir: %q
`, runDir)), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result, err := startDiagnosisRun(context.Background(), diagnoseOptions{
		Goal:       "检查骨架",
		ConfigPath: configPath,
	}, "skeleton")
	if err != nil {
		t.Fatalf("start diagnosis: %v", err)
	}
	if err := saveDiagnosisResult(result, nil); err != nil {
		t.Fatalf("save diagnosis: %v", err)
	}

	if _, err := runstore.NewStore(runDir).Load(result.State.RunID); err != nil {
		t.Fatalf("load run from configured dir: %v", err)
	}
}

func TestRunStatusAndReportLoadPersistedRun(t *testing.T) {
	runDir := t.TempDir()
	state := runstore.State{
		RunID:  "run_done",
		Goal:   "检查后端",
		Status: runstore.StatusCompleted,
		Plan: schema.Plan{
			Reason: "检查服务状态",
			Items: []schema.PlanItem{
				{ID: "backend", Goal: "检查后端", Status: "done"},
			},
		},
		Diagnosis: &schema.Diagnosis{
			Summary: "后端存活",
			Evidence: []schema.Evidence{
				{Step: 1, Tool: "http_check", Summary: "model summary"},
			},
		},
		Trace: []trace.Entry{
			{
				Step:       1,
				PlanItemID: "backend",
				ToolName:   "http_check",
				Result: schema.Observation{
					Tool:    "http_check",
					Summary: "returned 200",
				},
				Duration: time.Millisecond,
			},
		},
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(20, 0).UTC(),
	}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save run: %v", err)
	}

	status, err := readDiagnosisStatus(runOptions{RunID: "run_done", RunDir: runDir})
	if err != nil {
		t.Fatalf("run status: %v", err)
	}
	for _, want := range []string{`"run_id": "run_done"`, `"status": "completed"`, `"id": "backend"`, `"trace_steps": 1`} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q:\n%s", want, status)
		}
	}

	markdown, err := renderDiagnosisReport(runOptions{RunID: "run_done", RunDir: runDir})
	if err != nil {
		t.Fatalf("run report: %v", err)
	}
	for _, want := range []string{"后端存活", "Step 1 `http_check`", "returned 200"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("report missing %q:\n%s", want, markdown)
		}
	}
}

func TestResumeDiagnosisRunContinuesPersistedRunWithExistingTrace(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	runDir := t.TempDir()
	state := runstore.State{
		RunID:  "run_failed",
		Goal:   "检查后端是否存活",
		Status: runstore.StatusFailed,
		Plan: schema.Plan{
			Reason: "先检查后端",
			Items: []schema.PlanItem{
				{ID: "backend", Goal: "检查后端是否存活", Status: "done"},
			},
		},
		Trace: []trace.Entry{
			{
				Step:       1,
				PlanItemID: "backend",
				ToolName:   "http_check",
				Result: schema.Observation{
					Tool:    "http_check",
					Summary: "returned 200",
					Data: map[string]any{
						"status": 200,
					},
				},
				Duration: time.Millisecond,
			},
		},
		Error:     "previous interruption",
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(20, 0).UTC(),
	}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save run: %v", err)
	}

	var gotRequest openAIChatCompletionRequestForTest
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}
		content := `{"type":"final","thought_summary":"existing trace is enough","final":{"summary":"resume completed","root_cause":{"status":"undetermined"},"evidence":[{"step":1,"tool":"http_check","summary":"backend returned ok"}],"coverage":[{"plan_item_id":"backend","status":"done","evidence":[{"step":1,"tool":"http_check","summary":"backend returned ok"}]}]}}`
		if strings.Contains(gotRequest.Messages[1].Content, `"mode": "plan"`) {
			content = `{"plan":null}`
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]string{
						"content": content,
					},
				},
			},
		}); err != nil {
			t.Fatalf("write response: %v", err)
		}
	}))
	defer llmServer.Close()
	t.Setenv("OPENAI_BASE_URL", llmServer.URL)

	result, err := resumeDiagnosisRun(context.Background(), resumeOptions{
		RunID:       "run_failed",
		RunDir:      runDir,
		ConfigPath:  writeTestConfig(t, "agent:\n  skill_path: ../../skills/sre-diagnosis/SKILL.md\n"),
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, "")
	if err != nil {
		t.Fatalf("run resume: %v", err)
	}

	if result.State.RunID != "run_failed" {
		t.Fatalf("run id = %q, want run_failed", result.State.RunID)
	}
	if result.State.Status != runstore.StatusCompleted {
		t.Fatalf("status = %q, want completed", result.State.Status)
	}
	if result.State.Error != "" {
		t.Fatalf("error = %q, want empty", result.State.Error)
	}
	contextMessage := gotRequest.Messages[1].Content
	for _, want := range []string{`"step": 2`, "returned 200", `"status": 200`, `"id": "backend"`, `"plan_item_id": "backend"`} {
		if !strings.Contains(contextMessage, want) {
			t.Fatalf("resume request missing %q:\n%s", want, contextMessage)
		}
	}
	if !strings.Contains(result.Markdown, "resume completed") {
		t.Fatalf("markdown missing final summary:\n%s", result.Markdown)
	}
	if err := saveDiagnosisResult(result, nil); err != nil {
		t.Fatalf("save resumed run: %v", err)
	}

	loaded, err := runstore.NewStore(runDir).Load("run_failed")
	if err != nil {
		t.Fatalf("load resumed run: %v", err)
	}
	if loaded.Status != runstore.StatusCompleted {
		t.Fatalf("saved status = %q, want completed", loaded.Status)
	}
	if loaded.Diagnosis == nil || loaded.Diagnosis.Summary != "resume completed" {
		t.Fatalf("saved diagnosis = %#v", loaded.Diagnosis)
	}
}

func TestResumeDiagnosisRunRejectsMaxStepsAlreadyReached(t *testing.T) {
	runDir := t.TempDir()
	state := runstore.State{
		RunID:  "run_failed",
		Goal:   "检查后端是否存活",
		Status: runstore.StatusFailed,
		Trace: []trace.Entry{
			{Step: 2, ToolName: "http_check"},
		},
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(20, 0).UTC(),
	}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save run: %v", err)
	}

	_, err := resumeDiagnosisRun(context.Background(), resumeOptions{
		RunID:       "run_failed",
		RunDir:      runDir,
		ConfigPath:  writeTestConfig(t, ""),
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, "skeleton")
	if err == nil {
		t.Fatal("resume succeeded, want max steps error")
	}
	if !strings.Contains(err.Error(), "max steps 2 already reached by existing trace step 2") {
		t.Fatalf("error = %q, want max steps reached detail", err.Error())
	}
}

func TestResumeDiagnosisRunMarksInterruptedSideEffectUnknownAndBlocksRecovery(t *testing.T) {
	runDir := t.TempDir()
	state := runstore.State{
		RunID:     "run_unknown_effect",
		Goal:      "check synthetic transaction",
		Status:    runstore.StatusFailed,
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(11, 0).UTC(),
		Calls: []runstore.Call{{
			CallID:     "call_smoke",
			Kind:       runstore.CallKindTool,
			ToolName:   "smoke_run",
			SideEffect: true,
			Status:     runstore.CallStatusRunning,
			StartedAt:  time.Unix(10, 0).UTC(),
		}},
	}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save interrupted run: %v", err)
	}

	_, err := resumeDiagnosisRun(context.Background(), resumeOptions{
		RunID:      state.RunID,
		RunDir:     runDir,
		ConfigPath: writeTestConfig(t, ""),
	}, "skeleton")
	if err == nil || !strings.Contains(err.Error(), "automatic resume is blocked") {
		t.Fatalf("resume error = %v, want unknown side-effect block", err)
	}
	loaded, err := runstore.NewStore(runDir).Load(state.RunID)
	if err != nil {
		t.Fatalf("load recovered run: %v", err)
	}
	if loaded.Calls[0].Status != runstore.CallStatusUnknown || loaded.Calls[0].ErrorClass != runstore.ErrorClassUnknown {
		t.Fatalf("recovered call = %#v", loaded.Calls[0])
	}
}

func TestResumeDiagnosisRunRequiresConfirmationForStaleRunningState(t *testing.T) {
	runDir := t.TempDir()
	state := runstore.State{
		RunID:     "run_stale",
		Goal:      "check stale run",
		Status:    runstore.StatusRunning,
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(11, 0).UTC(),
	}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save stale run: %v", err)
	}
	options := resumeOptions{RunID: state.RunID, RunDir: runDir, ConfigPath: writeTestConfig(t, ""), MaxSteps: 1}
	if _, err := resumeDiagnosisRun(context.Background(), options, "skeleton"); err == nil || !strings.Contains(err.Error(), "--resume-running") {
		t.Fatalf("unconfirmed resume error = %v", err)
	}
	options.ResumeRunning = true
	result, err := resumeDiagnosisRun(context.Background(), options, "skeleton")
	if err != nil {
		t.Fatalf("confirmed resume: %v", err)
	}
	if result.State.Status != runstore.StatusCompleted || result.State.RunID != state.RunID {
		t.Fatalf("confirmed resume result = %#v", result.State)
	}
}

func TestCancelledDiagnosisPersistsTerminalCheckpoint(t *testing.T) {
	runDir := t.TempDir()
	sessionDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := startDiagnosisRun(ctx, diagnoseOptions{
		Goal:       "cancel before external work",
		ConfigPath: writeTestConfig(t, ""),
		RunDir:     runDir,
		SessionDir: sessionDir,
	}, "skeleton")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v, want context cancelled", err)
	}
	if result.State.Status != runstore.StatusCancelled || result.State.ErrorClass != runstore.ErrorClassCancelled {
		t.Fatalf("cancelled result = %#v", result.State)
	}
	if err := saveDiagnosisResult(result, err); err == nil {
		t.Fatal("save diagnosis returned nil after cancellation")
	}
	loaded, loadErr := runstore.NewStore(runDir).Load(result.State.RunID)
	if loadErr != nil {
		t.Fatalf("load cancelled checkpoint: %v", loadErr)
	}
	if loaded.Status != runstore.StatusCancelled || len(loaded.Calls) != 0 {
		t.Fatalf("saved cancelled state = %#v", loaded)
	}
}

func TestTaskTimeoutPersistsCallStatesAndTerminalStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "agent.log")
	if err := os.WriteFile(logFile, []byte("no log read expected\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	runDir := t.TempDir()
	sessionDir := t.TempDir()
	configPath := writeTestConfig(t, fmt.Sprintf("targets:\n  backend_base_url: %q\n  log_file: %q\n", server.URL, logFile))
	result, err := startDiagnosisRun(context.Background(), diagnoseOptions{
		Goal:        "enforce whole task budget",
		ConfigPath:  configPath,
		RunDir:      runDir,
		SessionDir:  sessionDir,
		MaxSteps:    3,
		ToolTimeout: time.Second,
		TaskTimeout: 20 * time.Millisecond,
	}, "login-500")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start error = %v, want task deadline", err)
	}
	if result.State.Status != runstore.StatusTimedOut || result.State.ErrorClass != runstore.ErrorClassDeadlineExceeded || result.State.TaskDeadline == nil || result.State.TaskDeadline.IsZero() {
		t.Fatalf("timed-out result = %#v", result.State)
	}
	if err := saveDiagnosisResult(result, err); err == nil {
		t.Fatal("save diagnosis returned nil after timeout")
	}
	loaded, loadErr := runstore.NewStore(runDir).Load(result.State.RunID)
	if loadErr != nil {
		t.Fatalf("load timed-out checkpoint: %v", loadErr)
	}
	if len(loaded.Calls) < 2 || loaded.Calls[0].CallID == "" || loaded.Calls[1].Kind != runstore.CallKindTool || loaded.Calls[1].Status != runstore.CallStatusFailed {
		t.Fatalf("saved calls = %#v", loaded.Calls)
	}
	status, statusErr := readDiagnosisStatus(runOptions{RunID: loaded.RunID, RunDir: runDir})
	if statusErr != nil || !strings.Contains(status, `"error_class": "deadline_exceeded"`) || !strings.Contains(status, `"call_id": "`) {
		t.Fatalf("status = %q, error = %v", status, statusErr)
	}
}

func TestResumeDiagnosisRunRejectsMissingCreatedAt(t *testing.T) {
	runDir := t.TempDir()
	if err := runstore.NewStore(runDir).Save(runstore.State{
		RunID: "run_failed",
		Goal:  "检查后端是否存活",
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	_, err := resumeDiagnosisRun(context.Background(), resumeOptions{RunID: "run_failed", RunDir: runDir}, "")
	if err == nil || !strings.Contains(err.Error(), "missing created_at") {
		t.Fatalf("error = %v, want missing created_at error", err)
	}
}

func TestResumeDiagnosisRunRejectsMissingGoal(t *testing.T) {
	runDir := t.TempDir()
	if err := runstore.NewStore(runDir).Save(runstore.State{
		RunID:     "run_failed",
		CreatedAt: time.Unix(10, 0).UTC(),
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	_, err := resumeDiagnosisRun(context.Background(), resumeOptions{RunID: "run_failed", RunDir: runDir}, "")
	if err == nil || !strings.Contains(err.Error(), "missing goal") {
		t.Fatalf("error = %v, want missing goal error", err)
	}
}

func TestResumeDiagnosisRunRejectsInvalidStatus(t *testing.T) {
	runDir := t.TempDir()
	if err := runstore.NewStore(runDir).Save(runstore.State{
		RunID:     "run_invalid",
		Goal:      "检查后端是否存活",
		CreatedAt: time.Unix(10, 0).UTC(),
	}); err != nil {
		t.Fatalf("save run: %v", err)
	}

	_, err := resumeDiagnosisRun(context.Background(), resumeOptions{RunID: "run_invalid", RunDir: runDir}, "")
	if err == nil || !strings.Contains(err.Error(), "unsupported status") {
		t.Fatalf("error = %v, want unsupported status error", err)
	}
}

func TestResumeDiagnosisRunRejectsMismatchedRunID(t *testing.T) {
	runDir := t.TempDir()
	state := runstore.State{
		RunID:     "other_run",
		Goal:      "检查后端是否存活",
		Status:    runstore.StatusFailed,
		CreatedAt: time.Unix(10, 0).UTC(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("encode run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "expected_run.json"), data, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}

	_, err = resumeDiagnosisRun(context.Background(), resumeOptions{RunID: "expected_run", RunDir: runDir}, "")
	if err == nil || !strings.Contains(err.Error(), "mismatched run_id") {
		t.Fatalf("error = %v, want mismatched run id error", err)
	}
}

func TestSaveDiagnosisResultIncludesRunIDWhenStateWasCreated(t *testing.T) {
	err := saveDiagnosisResult(diagnoseResult{
		State:  runstore.State{RunID: "run_failed"},
		RunDir: t.TempDir(),
	}, fmt.Errorf("max steps reached"))

	for _, want := range []string{"run_id: run_failed", "max steps reached"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%s", want, err)
		}
	}
}

func TestRunDiagnoseDependencyCheckScenarioExecutesPostgresAndRedisTools(t *testing.T) {
	postgresAddr := startFakePostgres(t)
	redisAddr := startFakeRedis(t)
	configPath := writeTestConfig(t, fmt.Sprintf(`
targets:
  postgres_dsn: %q
  redis_addr: %q
`, "postgres://app:secret@"+postgresAddr+"/chat_proj?sslmode=disable", redisAddr))

	markdown, err := diagnoseOnce(context.Background(), diagnoseOptions{
		Goal:        "检查依赖",
		ConfigPath:  configPath,
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}, "dependency-check")
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	for _, want := range []string{
		"依赖连通性检查完成",
		"Step 1 `postgres_ping`",
		"Step 2 `redis_ping`",
		"authentication_ok",
		"responded PONG",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunDiagnoseWebSocketScenarioExecutesWebSocketAndLogTools(t *testing.T) {
	wsAddr := startFakeWebSocket(t, http.StatusInternalServerError)
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "chat_proj.log")
	if err := os.WriteFile(logFile, []byte("INFO start\nERROR websocket upgrade failed: missing Authorization header\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	configPath := writeTestConfig(t, fmt.Sprintf(`
targets:
  websocket_url: %q
  log_file: %q
`, "ws://"+wsAddr+"/ws", logFile))

	markdown, err := diagnoseOnce(context.Background(), diagnoseOptions{
		Goal:        "诊断 WebSocket",
		ConfigPath:  configPath,
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}, "websocket")
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	for _, want := range []string{
		"WebSocket 诊断链路已完成",
		"Step 1 `websocket_check`",
		"Step 2 `log_read`",
		"handshake returned 500",
		`read 1 log lines matching "websocket"`,
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunDiagnoseUsesOpenAICompatibleChatClientFromEnvWhenNoMockScenario(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	var gotRequest openAIChatCompletionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}
		content := `{"type":"final","thought_summary":"smoke complete","final":{"summary":"真实 provider 已接入"}}`
		if strings.Contains(gotRequest.Messages[1].Content, `"mode": "plan"`) {
			content = `{"plan":null}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}})
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)
	configPath := writeTestConfig(t, `
agent:
  skill_path: ../../skills/sre-diagnosis/SKILL.md
targets:
  backend_base_url: "http://chat-proj.local:8080"
  log_file: "/var/log/chat_proj/app.log"
  postgres_dsn: "postgres://app:super-secret@db.local:5432/chat_proj?sslmode=disable"
  redis_addr: "redis.local:6379"
  websocket_url: "ws://chat-proj.local:8080/ws"
`)

	markdown, err := diagnoseOnce(context.Background(), diagnoseOptions{
		Goal:        "只做一次 provider 接入烟测",
		ConfigPath:  configPath,
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}, "")
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	if !strings.Contains(markdown, "真实 provider 已接入") {
		t.Fatalf("markdown missing real provider result:\n%s", markdown)
	}
	if len(gotRequest.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(gotRequest.Messages))
	}
	contextMessage := gotRequest.Messages[1].Content
	for _, want := range []string{
		"target_context",
		"backend_base_url",
		"http://chat-proj.local:8080",
		"allowed_post_urls",
		"/v1/user/login",
		"postgres_target",
		"postgres_dsn_configured",
		"db.local:5432",
		"redis.local:6379",
		"ws://chat-proj.local:8080/ws",
		"/var/log/chat_proj/app.log",
	} {
		if !strings.Contains(contextMessage, want) {
			t.Fatalf("model context missing %q:\n%s", want, contextMessage)
		}
	}
	if strings.Contains(contextMessage, "super-secret") {
		t.Fatalf("model context leaked postgres password:\n%s", contextMessage)
	}
	if strings.Contains(contextMessage, `"postgres_dsn"`) {
		t.Fatalf("model context should not expose postgres_dsn as a tool arg source:\n%s", contextMessage)
	}
}

func TestTargetContextDoesNotLeakMalformedPostgresDSN(t *testing.T) {
	context := targetContextForDiagnose(diagnoseOptions{
		PostgresDSN: "postgres://app:secret@db.local/%zz",
	})

	if context["postgres_dsn_configured"] != true {
		t.Fatalf("postgres_dsn_configured = %#v, want true", context["postgres_dsn_configured"])
	}
	if got := context["postgres_target"]; got != "[REDACTED_POSTGRES_DSN]" {
		t.Fatalf("postgres target = %#v, want redacted placeholder", got)
	}
}

func TestToolArgOverridesPinConfiguredDependencyTargets(t *testing.T) {
	overrides := toolArgOverridesForDiagnose(diagnoseOptions{
		PostgresDSN: "postgres://app:secret@db.local/chat",
		RedisAddr:   "redis.local:6379",
	})
	if got := overrides["postgres_check"]["dsn"]; got != "postgres://app:secret@db.local/chat" {
		t.Fatalf("postgres override = %#v", got)
	}
	if got := overrides["redis_ping"]["addr"]; got != "redis.local:6379" {
		t.Fatalf("redis override = %#v", got)
	}
}

func TestRunDiagnoseRealProviderCanDriveToolLoop(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	backendHits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits++
		if r.URL.Path != "/health" {
			t.Fatalf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer backend.Close()

	var gotRequests []openAIChatCompletionRequestForTest
	llmCalls := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var gotRequest openAIChatCompletionRequestForTest
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}
		gotRequests = append(gotRequests, gotRequest)

		content := ""
		switch llmCalls {
		case 1:
			content = `{"plan":null}`
		case 2:
			content = fmt.Sprintf(`{"type":"tool_call","thought_summary":"check backend health","tool":"http_check","args":{"url":%q}}`, backend.URL+"/health")
		case 3:
			content = `{"plan":null}`
		case 4:
			content = `{"type":"final","thought_summary":"backend evidence is enough","final":{"summary":"LLM tool loop complete","root_cause":{"status":"undetermined"},"evidence":[{"step":1,"tool":"http_check","summary":"backend returned ok"}]}}`
		default:
			t.Fatalf("unexpected llm call %d", llmCalls)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]string{
						"content": content,
					},
				},
			},
		}); err != nil {
			t.Fatalf("write llm response: %v", err)
		}
	}))
	defer llmServer.Close()
	t.Setenv("OPENAI_BASE_URL", llmServer.URL)

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	configPath := writeTestConfig(t, fmt.Sprintf(`
agent:
  skill_path: ../../skills/sre-diagnosis/SKILL.md
policy:
  allowed_hosts:
    - %q
targets:
  backend_base_url: %q
`, backendURL.Hostname(), backend.URL))
	markdown, err := diagnoseOnce(context.Background(), diagnoseOptions{
		Goal:        "检查后端是否存活",
		ConfigPath:  configPath,
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, "")
	if err != nil {
		t.Fatalf("run diagnose: %v", err)
	}

	if backendHits != 1 {
		t.Fatalf("backend hits = %d, want 1", backendHits)
	}
	if llmCalls != 4 {
		t.Fatalf("llm calls = %d, want 4", llmCalls)
	}
	if len(gotRequests) != 4 {
		t.Fatalf("captured llm requests = %d, want 4", len(gotRequests))
	}
	if !strings.Contains(gotRequests[0].Messages[1].Content, "target_context") {
		t.Fatalf("first request missing target context:\n%s", gotRequests[0].Messages[1].Content)
	}
	secondContext := gotRequests[3].Messages[1].Content
	for _, want := range []string{"http_check", "returned 200", `"status": 200`} {
		if !strings.Contains(secondContext, want) {
			t.Fatalf("second request missing %q:\n%s", want, secondContext)
		}
	}
	for _, want := range []string{"LLM tool loop complete", "Step 1 `http_check`", "returned 200"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func TestRunLLMChatReturnsRawModelContent(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	var gotRequest openAIChatCompletionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [
				{
					"message": {
						"content": "pong"
					}
				}
			]
		}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	content, err := chatWithLLM(context.Background(), "ping")
	if err != nil {
		t.Fatalf("run llm chat: %v", err)
	}

	if content != "pong" {
		t.Fatalf("content = %q, want pong", content)
	}
	if gotRequest.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q, want gpt-4o-mini", gotRequest.Model)
	}
	if gotRequest.ResponseFormat != nil {
		t.Fatalf("response_format = %#v, want nil for raw chat", gotRequest.ResponseFormat)
	}
	if len(gotRequest.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(gotRequest.Messages))
	}
	if gotRequest.Messages[0].Role != string(llm.RoleUser) {
		t.Fatalf("first role = %q, want user", gotRequest.Messages[0].Role)
	}
	if gotRequest.Messages[0].Content != "ping" {
		t.Fatalf("user message = %q, want ping", gotRequest.Messages[0].Content)
	}
}

func TestRunLLMPingUsesDefaultPingMessage(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "gpt-4o-mini")

	var gotRequest openAIChatCompletionRequestForTest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotRequest); err != nil {
			t.Fatalf("decode request: %v\n%s", err, body)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)

	content, err := pingLLM(context.Background())
	if err != nil {
		t.Fatalf("run llm ping: %v", err)
	}

	if content != "ok" {
		t.Fatalf("content = %q, want ok", content)
	}
	if len(gotRequest.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(gotRequest.Messages))
	}
	if !strings.Contains(gotRequest.Messages[0].Content, "pong") {
		t.Fatalf("ping message = %q, want pong instruction", gotRequest.Messages[0].Content)
	}
}

func TestResolveDiagnosisConfigLoadsConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
agent:
  max_steps: 11
  llm_timeout: 20s
  tool_timeout: 9s
  max_tool_calls: 18
  max_parallel_tools: 3
  context_budget_bytes: 16384
  tool_output_budget_bytes: 2048
policy:
  tool_allowlist:
    - http_check
    - log_read
  allowed_log_dirs:
    - /configured/logs
  allowed_hosts:
    - configured.local
    - localhost
  allowed_containers:
    - chat-backend
paths:
  run_dir: /configured/runs
  report_dir: /configured/reports
targets:
  backend_base_url: http://configured:8080
  postgres_dsn: postgres://configured:secret@db:5432/chat_proj?sslmode=disable
  redis_addr: redis:6379
  websocket_url: ws://configured/ws
  log_file: /configured/logs/app.log
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := resolveDiagnosisConfig(diagnoseOptions{
		Goal:       "diagnose",
		ConfigPath: configPath,
	})
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}

	if cfg.BackendBaseURL != "http://configured:8080" {
		t.Fatalf("backend base URL = %q, want config value", cfg.BackendBaseURL)
	}
	if cfg.LogFile != "/configured/logs/app.log" {
		t.Fatalf("log file = %q, want config value", cfg.LogFile)
	}
	if len(cfg.AllowedLogDirs) != 1 || cfg.AllowedLogDirs[0] != "/configured/logs" {
		t.Fatalf("allowed log dirs = %#v, want config value", cfg.AllowedLogDirs)
	}
	if len(cfg.AllowedHosts) != 2 || cfg.AllowedHosts[0] != "configured.local" || cfg.AllowedHosts[1] != "localhost" {
		t.Fatalf("allowed hosts = %#v, want configured.local/localhost", cfg.AllowedHosts)
	}
	if len(cfg.AllowedContainers) != 1 || cfg.AllowedContainers[0] != "chat-backend" {
		t.Fatalf("allowed containers = %#v, want chat-backend", cfg.AllowedContainers)
	}
	if cfg.PostgresDSN != "postgres://configured:secret@db:5432/chat_proj?sslmode=disable" {
		t.Fatalf("postgres dsn = %q, want config value", cfg.PostgresDSN)
	}
	if cfg.RedisAddr != "redis:6379" {
		t.Fatalf("redis addr = %q, want config value", cfg.RedisAddr)
	}
	if cfg.WebSocketURL != "ws://configured/ws" {
		t.Fatalf("websocket URL = %q, want config value", cfg.WebSocketURL)
	}
	if cfg.MaxSteps != 11 {
		t.Fatalf("max steps = %d, want 11", cfg.MaxSteps)
	}
	if cfg.LLMTimeout != 20*time.Second {
		t.Fatalf("llm timeout = %s, want 20s", cfg.LLMTimeout)
	}
	if cfg.ToolTimeout != 9*time.Second {
		t.Fatalf("tool timeout = %s, want 9s", cfg.ToolTimeout)
	}
	if cfg.MaxToolCalls != 18 || cfg.MaxParallelTools != 3 || cfg.ContextBudgetBytes != 16384 || cfg.ToolOutputBudgetBytes != 2048 {
		t.Fatalf("runtime limits = %d/%d/%d/%d", cfg.MaxToolCalls, cfg.MaxParallelTools, cfg.ContextBudgetBytes, cfg.ToolOutputBudgetBytes)
	}
	if cfg.RunDir != "/configured/runs" {
		t.Fatalf("run dir = %q, want config value", cfg.RunDir)
	}
	if cfg.ReportDir != "/configured/reports" {
		t.Fatalf("report dir = %q, want config value", cfg.ReportDir)
	}
}

func TestResolveDiagnosisConfigLoadsDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatalf("create configs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "config.yaml"), []byte(`
targets:
  backend_base_url: http://automatic:8080
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("change working dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	cfg, err := resolveDiagnosisConfig(diagnoseOptions{Goal: "diagnose"})
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	if cfg.BackendBaseURL != "http://automatic:8080" {
		t.Fatalf("backend base URL = %q, want automatic config value", cfg.BackendBaseURL)
	}
}

func TestResolveDiagnosisConfigAppliesCLIOverridesOnly(t *testing.T) {
	configPath := writeTestConfig(t, `
agent:
  max_steps: 11
  tool_timeout: 9s
  max_tool_calls: 18
  max_parallel_tools: 3
  context_budget_bytes: 16384
  tool_output_budget_bytes: 2048
policy:
  tool_allowlist:
    - http_check
  allowed_log_dirs:
    - /configured/logs
  allowed_hosts:
    - configured.local
paths:
  run_dir: /configured/runs
  report_dir: /configured/reports
targets:
  backend_base_url: http://configured:8080
  postgres_dsn: postgres://configured:secret@db:5432/chat_proj?sslmode=disable
  redis_addr: redis:6379
  websocket_url: ws://configured/ws
  log_file: /configured/logs/app.log
`)

	cfg, err := resolveDiagnosisConfig(diagnoseOptions{
		Goal:                  "diagnose",
		ConfigPath:            configPath,
		BackendBaseURL:        "http://override:8080",
		LogFile:               "/override/logs/app.log",
		MaxSteps:              3,
		LLMTimeout:            1500 * time.Millisecond,
		ToolTimeout:           2 * time.Second,
		MaxToolCalls:          4,
		MaxParallelTools:      1,
		ContextBudgetBytes:    8192,
		ToolOutputBudgetBytes: 1024,
		RunDir:                "/override/runs",
		ReportDir:             "/override/reports",
	})
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}

	if cfg.BackendBaseURL != "http://configured:8080" || cfg.LogFile != "/configured/logs/app.log" {
		t.Fatalf("non-CLI targets were overridden: %q/%q", cfg.BackendBaseURL, cfg.LogFile)
	}
	if cfg.MaxSteps != 3 || cfg.LLMTimeout != 1500*time.Millisecond || cfg.ToolTimeout != 2*time.Second || cfg.MaxToolCalls != 4 || cfg.MaxParallelTools != 1 || cfg.ContextBudgetBytes != 8192 || cfg.ToolOutputBudgetBytes != 1024 {
		t.Fatalf("agent config = %#v, want CLI overrides", cfg)
	}
	if cfg.RunDir != "/override/runs" || cfg.ReportDir != "/configured/reports" {
		t.Fatalf("paths = %q/%q, want CLI run dir and configured report dir", cfg.RunDir, cfg.ReportDir)
	}
}

func TestResolveDiagnosisConfigRejectsInvalidRuntimeLimitOverrides(t *testing.T) {
	configPath := writeTestConfig(t, "targets:\n  backend_base_url: http://configured:8080\n")
	for name, options := range map[string]diagnoseOptions{
		"parallelism": {Goal: "diagnose", ConfigPath: configPath, MaxParallelTools: 9},
		"context":     {Goal: "diagnose", ConfigPath: configPath, ContextBudgetBytes: 1023},
		"tool output": {Goal: "diagnose", ConfigPath: configPath, ToolOutputBudgetBytes: 511},
	} {
		if _, err := resolveDiagnosisConfig(options); err == nil {
			t.Fatalf("%s override succeeded, want runtime limit error", name)
		}
	}
}

func TestRunStatusAndReportUseConfiguredRunDir(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "runs")
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(`
paths:
  run_dir: %q
`, runDir)), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	state := runstore.State{
		RunID:  "run_done",
		Goal:   "检查后端",
		Status: runstore.StatusCompleted,
		Diagnosis: &schema.Diagnosis{
			Summary: "后端存活",
		},
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(20, 0).UTC(),
	}
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save run: %v", err)
	}

	status, err := readDiagnosisStatus(runOptions{RunID: "run_done", ConfigPath: configPath})
	if err != nil {
		t.Fatalf("run status: %v", err)
	}
	if !strings.Contains(status, `"run_id": "run_done"`) {
		t.Fatalf("status missing run id:\n%s", status)
	}

	markdown, err := renderDiagnosisReport(runOptions{RunID: "run_done", ConfigPath: configPath})
	if err != nil {
		t.Fatalf("run report: %v", err)
	}
	if !strings.Contains(markdown, "后端存活") {
		t.Fatalf("report missing summary:\n%s", markdown)
	}
}

func TestMarkdownOutputWritesReportAndReturnsMarkdown(t *testing.T) {
	reportDir := t.TempDir()
	result := diagnoseResult{
		Markdown:  "# report\n\nok\n",
		ReportDir: reportDir,
		State:     runstore.State{RunID: "run_1"},
	}

	markdown, err := markdownOutput("", result)
	if err != nil {
		t.Fatalf("markdown output: %v", err)
	}
	if markdown != result.Markdown {
		t.Fatalf("markdown = %q, want original report", markdown)
	}

	data, err := os.ReadFile(filepath.Join(reportDir, "run_1.md"))
	if err != nil {
		t.Fatalf("read written report: %v", err)
	}
	if string(data) != result.Markdown {
		t.Fatalf("written report = %q, want markdown", data)
	}
	info, err := os.Stat(filepath.Join(reportDir, "run_1.md"))
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("report mode = %o, want 600", got)
	}
}

type openAIChatCompletionRequestForTest struct {
	Model          string                       `json:"model"`
	Messages       []openAIChatMessageForTest   `json:"messages"`
	ResponseFormat *openAIResponseFormatForTest `json:"response_format,omitempty"`
}

type openAIChatMessageForTest struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFormatForTest struct {
	Type string `json:"type"`
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func clearLLMEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("MIMO_API_KEY", "")
	t.Setenv("MIMO_BASE_URL", "")
	t.Setenv("MIMO_MODEL", "")
	t.Setenv("OLLAMA_BASE_URL", "")
	t.Setenv("OLLAMA_MODEL", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
}

func TestNewChatClientSelectsConfiguredProtocol(t *testing.T) {
	openAIClient, err := newChatClient(llm.Config{Provider: "ollama", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("new ollama client: %v", err)
	}
	if _, ok := openAIClient.(*llm.OpenAICompatibleChatClient); !ok {
		t.Fatalf("ollama client type = %T", openAIClient)
	}

	claudeClient, err := newChatClient(llm.Config{Provider: "anthropic", APIKey: "test", Model: "claude-test"})
	if err != nil {
		t.Fatalf("new anthropic client: %v", err)
	}
	if _, ok := claudeClient.(*llm.AnthropicChatClient); !ok {
		t.Fatalf("anthropic client type = %T", claudeClient)
	}
}

func startFakePostgres(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen postgres: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		lengthBytes := make([]byte, 4)
		if _, err := io.ReadFull(conn, lengthBytes); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(lengthBytes)
		payload := make([]byte, int(length)-4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		_, _ = conn.Write([]byte{'R', 0, 0, 0, 8, 0, 0, 0, 0})
	}()

	return listener.Addr().String()
}

func startFakeWebSocket(t *testing.T, status int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen websocket: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		_, err = http.ReadRequest(reader)
		if err != nil {
			return
		}
		reason := http.StatusText(status)
		if reason == "" {
			reason = "status"
		}
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 14\r\n\r\nupgrade failed", status, reason)
	}()

	return listener.Addr().String()
}

func startFakeRedis(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen redis: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for i := 0; i < 5; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "PING" {
				break
			}
		}
		_, _ = conn.Write([]byte("+PONG\r\n"))
	}()

	return listener.Addr().String()
}
