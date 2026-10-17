package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	"github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
)

func TestDiagnosisLoadsModelSelectedMemory(t *testing.T) {
	clearLLMEnv(t)
	t.Setenv("SRE_AGENT_LLM_PROVIDER", "openai_compatible")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	runDir, memoryDir := t.TempDir(), t.TempDir()
	store := memory.NewStore(memoryDir).WithRunDir(runDir)
	historical := interactiveMemoryRun("run_worker", "session_previous", "Redis", time.Now())
	if err := runstore.NewStore(runDir).Save(historical); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateForRun(historical); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Process(context.Background(), &workerChat{}, memory.ProcessOptions{RunDir: runDir, ExtractModel: "test", ConsolidationModel: "test", Limit: 2, Timeout: time.Second})
	if err != nil || stats.Success != 2 {
		t.Fatalf("setup: %+v %v", stats, err)
	}
	var navigation, handbook, diagnosis atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIChatCompletionRequestForTest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		system := req.Messages[0].Content
		response := `{"type":"final","thought_summary":"done","final":{"summary":"检查完成"}}`
		switch {
		case strings.Contains(system, "Select relevant historical topics"):
			navigation.Add(1)
			response = `{"topics":[0]}`
		case strings.Contains(system, "Read the selected MEMORY.md"):
			handbook.Add(1)
			response = `{"runs":[{"run_id":"run_worker","need_rollout":false}]}`
		default:
			diagnosis.Add(1)
			if !strings.Contains(req.Messages[1].Content, "Verify identity") || !strings.Contains(req.Messages[1].Content, "run_worker") {
				t.Error("selected handbook was not injected into diagnosis")
			}
			if strings.Contains(req.Messages[1].Content, `"mode": "plan"`) {
				response = `{"plan":null}`
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": response}}}})
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL)
	configPath := writeTestConfig(t, `agent:
  skill_path: ../../skills/sre-diagnosis/SKILL.md
memory:
  model_enabled: true
  timeout: 5s
targets:
  service: go-chat
  environment: local
`)
	loaded := 0
	_, err = startDiagnosisRun(context.Background(), diagnoseOptions{Goal: "客户端连接异常", ConfigPath: configPath, RunDir: runDir, MemoryDir: memoryDir, SessionDir: t.TempDir(), MaxSteps: 1, Progress: func(event agent.ProgressEvent) {
		if event.Kind == agent.ProgressMemoryLoaded {
			for _, m := range event.Memories {
				if m.SourceRunID == "run_worker" {
					loaded++
				}
			}
		}
	}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if navigation.Load() != 1 || handbook.Load() != 1 || diagnosis.Load() == 0 || loaded != 1 {
		t.Fatalf("retrieval not wired: navigation=%d handbook=%d diagnosis=%d loaded=%d", navigation.Load(), handbook.Load(), diagnosis.Load(), loaded)
	}
}
