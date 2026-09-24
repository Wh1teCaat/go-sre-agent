package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
)

type workerChat struct {
	started chan struct{}
	block   bool
}

func (f *workerChat) Chat(ctx context.Context, req llm.ChatRequest) (string, error) {
	if f.block {
		select {
		case f.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	if strings.Contains(req.Messages[0].Content, "Extract") {
		return fmt.Sprintf(`{"candidates":[{"text":"Verify Redis identity","applicability":"local","sources":[{"run_id":"run_worker","step":1,"tool":"redis_ping","call_id":"call_run_worker"}]}]}`), nil
	}
	return `{"items":[{"topic":"Redis","knowledge":"Verify identity","applicability":"local","conflicts":[],"source_run_ids":["run_worker"]}]}`, nil
}
func TestMemoryWorkerDoesNotBlockAndRestartResumes(t *testing.T) {
	runDir := t.TempDir()
	store := memory.NewStore(t.TempDir()).WithRunDir(runDir)
	state := interactiveMemoryRun("run_worker", "session_worker", "Redis", time.Now())
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateForRun(state); err != nil {
		t.Fatal(err)
	}
	blocked := &workerChat{started: make(chan struct{}, 1), block: true}
	opts := memory.ProcessOptions{ExtractModel: "fake", ConsolidationModel: "fake", Timeout: time.Hour, Limit: 2}
	worker := startMemoryWorker(store, runDir, blocked, opts)
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	notified := make(chan struct{})
	go func() { worker.Notify(); close(notified) }()
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("notify blocked input")
	}
	stopped := make(chan struct{})
	go func() { worker.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel model")
	}
	ready := &workerChat{}
	resumed := startMemoryWorker(store, runDir, ready, opts)
	defer resumed.Stop()
	select {
	case status := <-resumed.events:
		if !strings.Contains(status, "成功 2") {
			t.Fatalf("status = %s", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not resume")
	}
	matches, err := store.Search(memory.Query{Service: "go-chat", Environment: "local", MaxBytes: 20000})
	if err != nil || len(matches) != 1 || !strings.Contains(matches[0].Content, "历史模型候选经验") {
		t.Fatalf("resumed memory = %+v, %v", matches, err)
	}
}
