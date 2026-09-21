package smoke

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSmokeRunParsesPassAndFailLines(t *testing.T) {
	script := `echo "PASS  login"; echo "PASS  send message"; echo "FAIL  B receives push — timeout waiting for frame"; echo "RESULT: 1 FAILED — B receives push"; exit 1`
	tool := New([]string{"sh", "-c", script}, t.TempDir(), time.Minute)

	observation, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run smoke: %v", err)
	}
	if observation.Data["passed"] != 2 || observation.Data["failed"] != 1 {
		t.Fatalf("counts = %#v", observation.Data)
	}
	if !strings.Contains(observation.Summary, "first failure: FAIL  B receives push") {
		t.Fatalf("summary = %q", observation.Summary)
	}
	if observation.Data["result"] != "1 FAILED — B receives push" {
		t.Fatalf("result = %#v", observation.Data["result"])
	}
}

func TestSmokeRunAllPass(t *testing.T) {
	tool := New([]string{"sh", "-c", `echo "PASS  a"; echo "RESULT: ALL PASS"`}, t.TempDir(), time.Minute)

	observation, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("run smoke: %v", err)
	}
	if observation.Data["passed"] != 1 || observation.Data["failed"] != 0 || observation.Data["result"] != "ALL PASS" {
		t.Fatalf("data = %#v", observation.Data)
	}
}

func TestSmokeRunDistinguishesStartupFailure(t *testing.T) {
	tool := New([]string{"sh", "-c", "echo boom >&2; exit 2"}, t.TempDir(), time.Minute)

	observation, err := tool.Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "run smoke test") {
		t.Fatalf("error = %v, want startup failure", err)
	}
	if !strings.Contains(observation.Data["output"].(string), "boom") {
		t.Fatalf("output = %#v", observation.Data["output"])
	}
}

func TestSmokeSpecDeclaresTimeout(t *testing.T) {
	if got := New(nil, "", 0).Spec().Timeout; got != 120*time.Second {
		t.Fatalf("default timeout = %s, want 120s", got)
	}
	if got := New(nil, "", 30*time.Second).Spec().Timeout; got != 30*time.Second {
		t.Fatalf("timeout = %s, want 30s", got)
	}
	if !New(nil, "", 0).Spec().SideEffect {
		t.Fatal("smoke spec side_effect = false, want true")
	}
}
