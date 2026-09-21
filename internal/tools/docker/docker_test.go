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

func TestStatsToolReportsUsageAndRestartCount(t *testing.T) {
	var gotArgs [][]string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append(gotArgs, append([]string(nil), args...))
		if args[0] == "stats" {
			return []byte(`{"CPUPerc":"0.15%","MemPerc":"1.50%","MemUsage":"12MiB / 1GiB","PIDs":"8"}`), nil
		}
		return []byte("2\n"), nil
	}
	tool := &StatsTool{policy: newPolicy([]string{"chat-backend"}, runner)}

	observation, err := tool.Run(context.Background(), json.RawMessage(`{"container":"chat-backend"}`))
	if err != nil {
		t.Fatalf("run stats: %v", err)
	}
	if want := []string{"stats", "--no-stream", "--format", "{{json .}}", "chat-backend"}; !reflect.DeepEqual(gotArgs[0], want) {
		t.Fatalf("docker args = %#v, want %#v", gotArgs[0], want)
	}
	if observation.Data["cpu_percent"] != "0.15%" || observation.Data["mem_usage"] != "12MiB / 1GiB" {
		t.Fatalf("observation data = %#v", observation.Data)
	}
	if observation.Data["restart_count"] != "2" {
		t.Fatalf("restart_count = %#v, want 2", observation.Data["restart_count"])
	}
	if !strings.Contains(observation.Summary, "cpu=0.15%") || !strings.Contains(observation.Summary, "restarts=2") {
		t.Fatalf("summary = %q", observation.Summary)
	}
}

func TestProbeToolRunsFixedTemplateInsideContainer(t *testing.T) {
	var gotArgs []string
	runner := func(_ context.Context, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(`{"redis":"ok","status":"ok"}`), nil
	}
	tool := &ProbeTool{policy: newPolicy([]string{"go-chat-gateway-1"}, runner)}

	observation, err := tool.Run(context.Background(), json.RawMessage(`{"probe":"http_health","container":"go-chat-gateway-1","port":8081}`))
	if err != nil {
		t.Fatalf("run probe: %v", err)
	}
	want := []string{"exec", "go-chat-gateway-1", "sh", "-c", "wget -q -O- -T 4 http://localhost:8081/health"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("docker args = %#v, want %#v", gotArgs, want)
	}
	if !strings.Contains(observation.Summary, `"status":"ok"`) {
		t.Fatalf("summary = %q", observation.Summary)
	}
}

func TestProbeToolRejectsUnknownTemplateAndBadPort(t *testing.T) {
	called := false
	runner := func(_ context.Context, _ ...string) ([]byte, error) {
		called = true
		return nil, errors.New("must not run")
	}
	tool := &ProbeTool{policy: newPolicy([]string{"chat-edge"}, runner)}

	if _, err := tool.Run(context.Background(), json.RawMessage(`{"probe":"rm_rf","container":"chat-edge"}`)); err == nil || !strings.Contains(err.Error(), "unknown probe template") {
		t.Fatalf("error = %v, want unknown template", err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"probe":"http_health","container":"chat-edge"}`)); err == nil || !strings.Contains(err.Error(), "requires port") {
		t.Fatalf("error = %v, want port requirement", err)
	}
	if _, err := tool.Run(context.Background(), json.RawMessage(`{"probe":"nginx_config","container":"other"}`)); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("error = %v, want container allowlist rejection", err)
	}
	if called {
		t.Fatal("docker command ran for rejected probe")
	}
}

func TestProbeToolKeepsFailedOutputAsEvidence(t *testing.T) {
	runner := func(_ context.Context, _ ...string) ([]byte, error) {
		return []byte("wget: server returned error: HTTP/1.1 503 Service Unavailable"), errors.New("exit status 1")
	}
	tool := &ProbeTool{policy: newPolicy([]string{"go-chat-gateway-1"}, runner)}

	observation, err := tool.Run(context.Background(), json.RawMessage(`{"probe":"http_health","container":"go-chat-gateway-1","port":8081}`))
	if err == nil {
		t.Fatal("expected probe failure error")
	}
	if !strings.Contains(observation.Data["output"].(string), "503") {
		t.Fatalf("output = %#v, want 503 evidence", observation.Data["output"])
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
