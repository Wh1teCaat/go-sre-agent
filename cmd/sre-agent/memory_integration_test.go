package main

import (
	"testing"
	"time"

	memory "github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

// TestSaveDiagnosisResultUpdatesCrossSessionMemoryAfterRunAndSession 验证持久化顺序包含跨会话收录。
func TestSaveDiagnosisResultUpdatesCrossSessionMemoryAfterRunAndSession(t *testing.T) {
	runDir := t.TempDir()
	memoryDir := t.TempDir()
	state := runstore.State{
		RunID:       "run_memory_pipeline",
		Service:     "go-chat",
		Environment: "local",
		Goal:        "检查 Redis 连接",
		Status:      runstore.StatusCompleted,
		Trace: []trace.Entry{{
			Step:     1,
			CallID:   "call_memory_pipeline",
			ToolName: "redis_ping",
			Result:   schema.Observation{Tool: "redis_ping", Summary: "PONG"},
		}},
		Diagnosis: &schema.Diagnosis{
			Summary:   "Redis 可达。",
			RootCause: &schema.RootCause{Status: "undetermined"},
			Evidence:  []schema.Evidence{{Step: 1, Tool: "redis_ping"}},
		},
		CreatedAt: time.Unix(10, 0).UTC(),
		UpdatedAt: time.Unix(20, 0).UTC(),
	}
	if err := saveDiagnosisResult(diagnoseResult{
		State:       state,
		RunDir:      runDir,
		MemoryDir:   memoryDir,
		Service:     state.Service,
		Environment: state.Environment,
	}, nil); err != nil {
		t.Fatalf("save diagnosis result: %v", err)
	}
	hints, err := memory.NewStore(memoryDir).Hints(memory.Query{Service: "go-chat", Environment: "local"})
	if err != nil {
		t.Fatalf("load cross-session hints: %v", err)
	}
	if len(hints) != 1 || hints[0].SourceRunID != state.RunID {
		t.Fatalf("cross-session hints = %#v", hints)
	}
}
