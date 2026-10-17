package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestParallelResultsCommitBeforeSlowToolCompletes(t *testing.T) {
	for _, failCheckpoint := range []bool{false, true} {
		name := "completion order"
		if failCheckpoint {
			name = "checkpoint failure cancels remaining worker"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			registry := tools.NewRegistry()
			slow := &parallelRuntimeState{started: make(chan string, 1), release: make(chan struct{})}
			fast := &parallelRuntimeState{started: make(chan string, 1), release: make(chan struct{})}
			for _, tool := range []concurrentRuntimeTool{{name: "slow", state: slow}, {name: "fast", state: fast}} {
				if err := registry.Register(tool); err != nil {
					t.Fatal(err)
				}
			}
			snapshots := make(chan Checkpoint, 4)
			events := make(chan ProgressEvent, 4)
			checkpointErr := errors.New("checkpoint unavailable")
			store := new(trace.MemoryStore)
			runtime := NewRuntime(RuntimeConfig{MaxParallelTools: 2, ToolTimeout: time.Second * 5,
				Checkpoint: func(cp Checkpoint) error {
					if len(cp.Trace) > 0 {
						snapshots <- cp
						if failCheckpoint {
							return checkpointErr
						}
					}
					return nil
				},
				Progress: func(event ProgressEvent) {
					if event.Kind == ProgressCheckCompleted {
						events <- event
					}
				},
			}, nil, registry, nil, store)
			done := make(chan error, 1)
			go func() {
				done <- runtime.executeToolCalls(ctx, 1, []schema.ToolCall{{Tool: "slow", Args: json.RawMessage(`{}`)}, {Tool: "fast", Args: json.RawMessage(`{}`)}}, llmMeta{})
			}()
			for _, state := range []*parallelRuntimeState{slow, fast} {
				select {
				case <-state.started:
				case <-time.After(2 * time.Second):
					t.Fatal("tool did not start")
				}
			}
			close(fast.release)
			var first Checkpoint
			select {
			case first = <-snapshots:
			case <-time.After(2 * time.Second):
				t.Fatal("fast completion waited for slow tool")
			}
			if len(first.Trace) != 1 || first.Trace[0].ToolName != "fast" {
				t.Fatalf("first trace: %+v", first.Trace)
			}
			if first.Calls[0].Status != runstore.CallStatusRunning || first.Calls[1].Status != runstore.CallStatusSucceeded || first.Calls[1].CallID != first.Trace[0].CallID {
				t.Fatalf("partial checkpoint has wrong states or call identity: %+v", first)
			}
			if failCheckpoint {
				select {
				case err := <-done:
					if !errors.Is(err, checkpointErr) {
						t.Fatalf("error: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("remaining worker not cancelled/drained")
				}
				slow.mutex.Lock()
				running := slow.running
				slow.mutex.Unlock()
				if running != 0 {
					t.Fatal("worker still running after return")
				}
				if len(store.List()) != 1 {
					t.Fatal("committed after checkpoint failure")
				}
				select {
				case event := <-events:
					t.Fatalf("completion emitted before successful checkpoint: %+v", event)
				default:
				}
				return
			}
			select {
			case event := <-events:
				if event.Tool != "fast" {
					t.Fatalf("first completion: %+v", event)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("completion event delayed")
			}
			select {
			case err := <-done:
				t.Fatalf("batch ended while slow tool still running: %v", err)
			default:
			}
			close(slow.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("batch did not finish")
			}
			entries := store.List()
			if len(entries) != 2 || entries[0].ToolName != "fast" || entries[1].ToolName != "slow" {
				t.Fatalf("trace not in completion order: %+v", entries)
			}
			if event := <-events; event.Tool != "slow" {
				t.Fatalf("second completion: %+v", event)
			}
		})
	}
}
