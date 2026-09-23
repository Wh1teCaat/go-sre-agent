package main

import (
	"context"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	memory "github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
)

func TestActualMemoryLoadEventUsesDiagnosisHints(t *testing.T) {
	memoryDir := t.TempDir()
	historical := interactiveMemoryRun("run_history", "session_history", "检查 Redis 连接", time.Unix(10, 0).UTC())
	if _, err := memory.NewStore(memoryDir).UpdateForRun(historical); err != nil {
		t.Fatal(err)
	}
	var loaded [][]string
	result, err := startDiagnosisRun(context.Background(), diagnoseOptions{Goal: "检查 Redis 连接", ConfigPath: writeTestConfig(t, ""), RunDir: t.TempDir(), SessionDir: t.TempDir(), MemoryDir: memoryDir, Progress: func(e agent.ProgressEvent) {
		if e.Kind == agent.ProgressMemoryLoaded {
			sources := make([]string, 0, len(e.Memories))
			for _, m := range e.Memories {
				sources = append(sources, m.SourceRunID)
			}
			loaded = append(loaded, sources)
		}
	}}, "skeleton")
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Diagnosis == nil || len(loaded) != 1 || len(loaded[0]) != 1 || loaded[0][0] != "run_history" {
		t.Fatalf("result=%+v, loaded=%v", result.State.Diagnosis, loaded)
	}
}

func TestTUIDemoUsesOnlySimulatedTools(t *testing.T) {
	opts := diagnoseOptions{Goal: "登录接口返回 500", ConfigPath: "../../configs/tui-demo.yaml", RunDir: t.TempDir(), SessionDir: t.TempDir(), MemoryDir: t.TempDir()}
	result, err := startDiagnosisRun(context.Background(), opts, "tui-demo")
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Diagnosis == nil || len(result.State.Trace) != 4 {
		t.Fatalf("demo result=%+v", result.State)
	}
	health, detail, issue, cause := tuiAssessment(result)
	if health != "Unhealthy" || detail != "登录接口返回 500" || issue != "PostgreSQL 连接检查失败" || cause != "Not confirmed" {
		t.Fatalf("assessment = %q, %q, %q, %q", health, detail, issue, cause)
	}
	for _, entry := range result.State.Trace {
		if entry.ToolName != "" && entry.ToolName != "demo_http" && entry.ToolName != "demo_logs" && entry.ToolName != "demo_postgres" {
			t.Fatalf("unexpected real tool %q", entry.ToolName)
		}
	}
}

func TestTUIAssessmentDoesNotInferHealthFromModelSummary(t *testing.T) {
	result := diagnoseResult{State: runstore.State{Diagnosis: &schema.Diagnosis{
		Summary: "后端健康", RootCause: &schema.RootCause{Status: "undetermined"},
	}}}
	health, detail, issue, cause := tuiAssessment(result)
	if health != "Unverified" || detail != "" || issue != "" || cause != "Not confirmed" {
		t.Fatalf("assessment = %q, %q, %q, %q", health, detail, issue, cause)
	}
}
