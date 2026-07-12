package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const (
	PSName      = "docker_ps"
	InspectName = "docker_inspect"
	LogsName    = "docker_logs"
)

type InspectArgs struct {
	Container string `json:"container"`
}

type LogsArgs struct {
	Container string `json:"container"`
	Lines     int    `json:"lines,omitempty"`
}

type commandRunner func(ctx context.Context, args ...string) ([]byte, error)

type policy struct {
	allowed map[string]struct{}
	names   []string
	run     commandRunner
}

type PSTool struct{ policy *policy }
type InspectTool struct{ policy *policy }
type LogsTool struct{ policy *policy }

func NewPS(allowedContainers []string) *PSTool {
	return &PSTool{policy: newPolicy(allowedContainers, runDocker)}
}

func NewInspect(allowedContainers []string) *InspectTool {
	return &InspectTool{policy: newPolicy(allowedContainers, runDocker)}
}

func NewLogs(allowedContainers []string) *LogsTool {
	return &LogsTool{policy: newPolicy(allowedContainers, runDocker)}
}

func newPolicy(allowedContainers []string, runner commandRunner) *policy {
	allowed := make(map[string]struct{}, len(allowedContainers))
	for _, name := range allowedContainers {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	sort.Strings(names)
	return &policy{allowed: allowed, names: names, run: runner}
}

func (p *policy) validate(container string) (string, error) {
	container = strings.TrimSpace(container)
	if container == "" {
		return "", fmt.Errorf("container is required")
	}
	if _, ok := p.allowed[container]; !ok {
		return "", fmt.Errorf("container %q is not allowed", container)
	}
	return container, nil
}

func (t *PSTool) Name() string                  { return PSName }
func (t *PSTool) Description() string           { return PSSpec().Description }
func (t *PSTool) Schema() tools.ToolSchema      { return PSSpec().Schema }
func (t *InspectTool) Name() string             { return InspectName }
func (t *InspectTool) Description() string      { return InspectSpec().Description }
func (t *InspectTool) Schema() tools.ToolSchema { return InspectSpec().Schema }
func (t *LogsTool) Name() string                { return LogsName }
func (t *LogsTool) Description() string         { return LogsSpec().Description }
func (t *LogsTool) Schema() tools.ToolSchema    { return LogsSpec().Schema }

func (t *PSTool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	if len(t.policy.names) == 0 {
		return schema.Observation{}, fmt.Errorf("no allowed Docker containers configured")
	}
	states := make([]map[string]any, 0, len(t.policy.names))
	failed := 0
	for _, name := range t.policy.names {
		state, err := inspectState(ctx, t.policy.run, name)
		if err != nil {
			failed++
			states = append(states, map[string]any{"container": name, "error": tools.RedactSensitive(err.Error())})
			continue
		}
		states = append(states, stateData(name, state))
	}
	observation := schema.Observation{
		Tool:    PSName,
		Summary: fmt.Sprintf("inspected %d Docker containers: %d available, %d failed", len(states), len(states)-failed, failed),
		Data:    map[string]any{"containers": states},
	}
	if failed == len(states) {
		return observation, fmt.Errorf("inspect all allowed Docker containers failed")
	}
	return observation, nil
}

func (t *InspectTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args InspectArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode docker_inspect args: %w", err)
	}
	container, err := t.policy.validate(args.Container)
	if err != nil {
		return schema.Observation{}, err
	}
	state, err := inspectState(ctx, t.policy.run, container)
	if err != nil {
		return schema.Observation{}, err
	}
	health := "none"
	if state.Health != nil {
		health = state.Health.Status
	}
	return schema.Observation{
		Tool:    InspectName,
		Summary: fmt.Sprintf("Docker container %s status=%s running=%t exit_code=%d health=%s", container, state.Status, state.Running, state.ExitCode, health),
		Data:    stateData(container, state),
	}, nil
}

func (t *LogsTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args LogsArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode docker_logs args: %w", err)
	}
	container, err := t.policy.validate(args.Container)
	if err != nil {
		return schema.Observation{}, err
	}
	lines := args.Lines
	if lines <= 0 {
		lines = 100
	}
	if lines > 500 {
		lines = 500
	}
	output, err := t.policy.run(ctx, "logs", "--tail", strconv.Itoa(lines), container)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("docker logs %q: %w: %s", container, err, tools.RedactSensitive(strings.TrimSpace(string(output))))
	}
	text := tools.RedactSensitive(strings.TrimSpace(string(output)))
	logLines := []string{}
	if text != "" {
		logLines = strings.Split(text, "\n")
	}
	return schema.Observation{
		Tool:    LogsName,
		Summary: fmt.Sprintf("read %d log lines from Docker container %s", len(logLines), container),
		Data: map[string]any{
			"container":  container,
			"lines":      logLines,
			"lines_read": len(logLines),
		},
	}, nil
}

type containerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
	Health     *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

func inspectState(ctx context.Context, runner commandRunner, container string) (containerState, error) {
	output, err := runner(ctx, "inspect", "--format", "{{json .State}}", container)
	if err != nil {
		return containerState{}, fmt.Errorf("docker inspect %q: %w: %s", container, err, tools.RedactSensitive(strings.TrimSpace(string(output))))
	}
	var state containerState
	if err := json.Unmarshal(bytes.TrimSpace(output), &state); err != nil {
		return containerState{}, fmt.Errorf("decode docker inspect state for %q: %w", container, err)
	}
	return state, nil
}

func stateData(container string, state containerState) map[string]any {
	health := ""
	if state.Health != nil {
		health = state.Health.Status
	}
	return map[string]any{
		"container":   container,
		"status":      state.Status,
		"running":     state.Running,
		"exit_code":   state.ExitCode,
		"health":      health,
		"error":       tools.RedactSensitive(state.Error),
		"started_at":  state.StartedAt,
		"finished_at": state.FinishedAt,
	}
}

// cappedBuffer 截断 Docker CLI 输出，同时向 os/exec 报告完整写入，避免命令因短写失败。
type cappedBuffer struct {
	data []byte
	max  int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.max - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(remaining, len(p))]...)
	}
	return written, nil
}

func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	output := &cappedBuffer{max: 64 << 10}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	return output.data, err
}

func PSSpec() tools.ToolSpec {
	return tools.ToolSpec{Name: PSName, Description: "Inspect runtime state of every configured Docker container.", Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{}}}
}

func InspectSpec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: InspectName, Description: "Inspect state, exit code, and health of one configured Docker container.",
		Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
			"container": {Type: "string", Required: true, Description: "Exact configured container name."},
		}},
	}
}

func LogsSpec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: LogsName, Description: "Read recent logs from one configured Docker container.",
		Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
			"container": {Type: "string", Required: true, Description: "Exact configured container name."},
			"lines":     {Type: "number", Description: "Recent log line count, capped at 500."},
		}},
	}
}
