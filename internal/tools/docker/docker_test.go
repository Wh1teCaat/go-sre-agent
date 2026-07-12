package docker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestInspectToolReturnsContainerState(t *testing.T) {
	var gotArgs []string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(`{"Status":"exited","Running":false,"ExitCode":1,"Error":"","Health":{"Status":"unhealthy"}}`), nil
	}
	tool := &InspectTool{policy: newPolicy([]string{"chat-backend"}, runner)}

	observation, err := tool.Run(context.Background(), json.RawMessage(`{"container":"chat-backend"}`))
	if err != nil {
		t.Fatalf("run inspect: %v", err)
	}
	if want := []string{"inspect", "--format", "{{json .State}}", "chat-backend"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("docker args = %#v, want %#v", gotArgs, want)
	}
	if observation.Data["exit_code"] != 1 || observation.Data["health"] != "unhealthy" {
		t.Fatalf("observation data = %#v", observation.Data)
	}
}

func TestInspectToolRejectsContainerOutsideAllowlist(t *testing.T) {
	called := false
	runner := func(_ context.Context, _ ...string) ([]byte, error) {
		called = true
		return nil, errors.New("must not run")
	}
	tool := &InspectTool{policy: newPolicy([]string{"chat-backend"}, runner)}

	_, err := tool.Run(context.Background(), json.RawMessage(`{"container":"other"}`))
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v, want allowlist rejection", err)
	}
	if called {
		t.Fatal("docker command ran for disallowed container")
	}
}

func TestLogsToolCapsLinesAndRedactsSecrets(t *testing.T) {
	var gotArgs []string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("started\nAuthorization: Bearer top-secret"), nil
	}
	tool := &LogsTool{policy: newPolicy([]string{"chat-backend"}, runner)}

	observation, err := tool.Run(context.Background(), json.RawMessage(`{"container":"chat-backend","lines":999}`))
	if err != nil {
		t.Fatalf("run logs: %v", err)
	}
	if want := []string{"logs", "--tail", "500", "chat-backend"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("docker args = %#v, want %#v", gotArgs, want)
	}
	lines, ok := observation.Data["lines"].([]string)
	if !ok || len(lines) != 2 {
		t.Fatalf("log lines = %#v", observation.Data["lines"])
	}
	if strings.Contains(strings.Join(lines, "\n"), "top-secret") {
		t.Fatalf("secret was not redacted: %#v", lines)
	}
}
