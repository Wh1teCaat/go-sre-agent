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
	"sync"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const (
	PSName      = "docker_ps"
	InspectName = "docker_inspect"
	LogsName    = "docker_logs"
	StatsName   = "docker_stats"
)

type InspectArgs struct {
	Container string `json:"container"`
}

type LogsArgs struct {
	Container   string `json:"container"`
	Lines       int    `json:"lines,omitempty"`
	LastMinutes int    `json:"last_minutes,omitempty"`
	Keyword     string `json:"keyword,omitempty"`
}

type StatsArgs struct {
	Container string `json:"container"`
}

type commandRunner func(ctx context.Context, args ...string) ([]byte, error)

// Scope 将 Compose 项目与允许诊断的服务限定在运营者配置中。模型不能传入或改写
// 这些值；docker_ps 只缓存当前项目中属于允许服务的动态容器名。
type Scope struct {
	ComposeProject         string
	AllowedComposeServices []string
	discovered             *discoveredContainers
}

// NewScope 创建供同一次诊断中所有 Docker 工具共享的 Compose 范围。
// 参数: composeProject 为项目标签，allowedServices 为允许读取的服务；返回: 可复用范围。
func NewScope(composeProject string, allowedServices []string) Scope {
	return Scope{
		ComposeProject:         strings.TrimSpace(composeProject),
		AllowedComposeServices: append([]string(nil), allowedServices...),
		discovered:             &discoveredContainers{names: make(map[string]struct{})},
	}
}

type discoveredContainers struct {
	names map[string]struct{}
	mu    sync.RWMutex
}

type policy struct {
	allowed         map[string]struct{}
	names           []string
	composeProject  string
	composeServices map[string]struct{}
	discovered      *discoveredContainers
	run             commandRunner
}

type PSTool struct{ policy *policy }
type InspectTool struct{ policy *policy }
type LogsTool struct{ policy *policy }
type StatsTool struct{ policy *policy }

func NewPS(allowedContainers []string, scopes ...Scope) *PSTool {
	return &PSTool{policy: newPolicy(allowedContainers, runDocker, scopes...)}
}

func NewInspect(allowedContainers []string, scopes ...Scope) *InspectTool {
	return &InspectTool{policy: newPolicy(allowedContainers, runDocker, scopes...)}
}

func NewLogs(allowedContainers []string, scopes ...Scope) *LogsTool {
	return &LogsTool{policy: newPolicy(allowedContainers, runDocker, scopes...)}
}

func NewStats(allowedContainers []string, scopes ...Scope) *StatsTool {
	return &StatsTool{policy: newPolicy(allowedContainers, runDocker, scopes...)}
}

func newPolicy(allowedContainers []string, runner commandRunner, scopes ...Scope) *policy {
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
	discovered := &discoveredContainers{names: make(map[string]struct{})}
	policy := &policy{
		allowed:         allowed,
		names:           names,
		composeServices: make(map[string]struct{}),
		discovered:      discovered,
		run:             runner,
	}
	if len(scopes) > 0 {
		policy.composeProject = strings.TrimSpace(scopes[0].ComposeProject)
		if scopes[0].discovered != nil {
			policy.discovered = scopes[0].discovered
		}
		for _, service := range scopes[0].AllowedComposeServices {
			if service = strings.TrimSpace(service); service != "" {
				policy.composeServices[service] = struct{}{}
			}
		}
	}
	return policy
}

func (p *policy) validate(container string) (string, error) {
	container = strings.TrimSpace(container)
	if container == "" {
		return "", fmt.Errorf("container is required")
	}
	if _, ok := p.allowed[container]; !ok {
		p.discovered.mu.RLock()
		_, discovered := p.discovered.names[container]
		p.discovered.mu.RUnlock()
		if !discovered {
			return "", fmt.Errorf("container %q is not allowed", container)
		}
	}
	return container, nil
}

func (t *PSTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: PSName, Description: "Inspect configured Docker containers. When a Compose project is configured, discover its allowed service replicas first so generated container names remain valid after scaling or project renaming.", Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{}}}
}

func (t *InspectTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: InspectName, Description: "Inspect state, exit code, and health of one configured Docker container.",
		Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
			"container": {Type: "string", Required: true, Description: "Exact configured container name."},
		}},
	}
}

func (t *StatsTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: StatsName, Description: "Read one-shot CPU, memory, and PID usage plus restart count of one configured Docker container.",
		Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
			"container": {Type: "string", Required: true, Description: "Exact configured container name."},
		}},
	}
}

func (t *LogsTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: LogsName, Description: "Read recent logs from one configured Docker container.",
		Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
			"container":    {Type: "string", Required: true, Description: "Exact configured or docker_ps-discovered container name."},
			"lines":        {Type: "number", Description: "Recent log line count, capped at 500."},
			"last_minutes": {Type: "number", Description: "Optional recent time window in minutes (1-1440)."},
			"keyword":      {Type: "string", Description: "Optional case-insensitive keyword filter applied locally after Docker returns the bounded log tail."},
		}},
	}
}

func (t *PSTool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	names := t.policy.names
	if t.policy.composeProject != "" {
		var err error
		names, err = t.policy.discoverComposeContainers(ctx)
		if err != nil {
			return schema.Observation{}, err
		}
	}
	if len(names) == 0 {
		if t.policy.composeProject != "" {
			return schema.Observation{
				Tool:    PSName,
				Summary: fmt.Sprintf("Compose project %s has no discovered allowed containers", t.policy.composeProject),
				Data:    map[string]any{"compose_project": t.policy.composeProject, "containers": []map[string]any{}},
			}, nil
		}
		return schema.Observation{}, fmt.Errorf("no allowed Docker containers configured")
	}
	states := make([]map[string]any, 0, len(names))
	failed := 0
	for _, name := range names {
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
		Data:    map[string]any{"containers": states, "compose_project": t.policy.composeProject},
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
	lastMinutes := args.LastMinutes
	if lastMinutes < 0 || lastMinutes > 24*60 {
		return schema.Observation{}, fmt.Errorf("last_minutes must be 0 or between 1 and 1440")
	}
	keyword := strings.TrimSpace(args.Keyword)
	if len([]rune(keyword)) > 128 || strings.ContainsAny(keyword, "\r\n") {
		return schema.Observation{}, fmt.Errorf("keyword must contain at most 128 characters and no line breaks")
	}
	command := []string{"logs", "--tail", strconv.Itoa(lines)}
	if lastMinutes > 0 {
		command = append(command, "--since", strconv.Itoa(lastMinutes)+"m")
	}
	command = append(command, container)
	output, err := t.policy.run(ctx, command...)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("docker logs %q: %w: %s", container, err, tools.RedactSensitive(strings.TrimSpace(string(output))))
	}
	text := tools.RedactSensitive(strings.TrimSpace(string(output)))
	logLines := []string{}
	if text != "" {
		logLines = strings.Split(text, "\n")
	}
	if keyword != "" {
		logLines = filterLines(logLines, keyword)
	}
	summary := fmt.Sprintf("read %d log lines from Docker container %s", len(logLines), container)
	if keyword != "" {
		summary += fmt.Sprintf(" matching %q", keyword)
	}
	return schema.Observation{
		Tool:    LogsName,
		Summary: summary,
		Data: map[string]any{
			"container":    container,
			"lines":        logLines,
			"lines_read":   len(logLines),
			"last_minutes": lastMinutes,
			"keyword":      keyword,
		},
	}, nil
}

// filterLines 按不区分大小写的关键词保留日志行，避免模型上下文被无关容器输出占满。
func filterLines(lines []string, keyword string) []string {
	needle := strings.ToLower(keyword)
	matched := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), needle) {
			matched = append(matched, line)
		}
	}
	return matched
}

func (t *StatsTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args StatsArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode docker_stats args: %w", err)
	}
	container, err := t.policy.validate(args.Container)
	if err != nil {
		return schema.Observation{}, err
	}
	output, err := t.policy.run(ctx, "stats", "--no-stream", "--format", "{{json .}}", container)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("docker stats %q: %w: %s", container, err, tools.RedactSensitive(strings.TrimSpace(string(output))))
	}
	var stats struct {
		CPUPerc  string `json:"CPUPerc"`
		MemPerc  string `json:"MemPerc"`
		MemUsage string `json:"MemUsage"`
		PIDs     string `json:"PIDs"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &stats); err != nil {
		return schema.Observation{}, fmt.Errorf("decode docker stats for %q: %w", container, err)
	}

	data := map[string]any{
		"container":   container,
		"cpu_percent": stats.CPUPerc,
		"mem_percent": stats.MemPerc,
		"mem_usage":   stats.MemUsage,
		"pids":        stats.PIDs,
	}
	summary := fmt.Sprintf("Docker container %s cpu=%s mem=%s (%s) pids=%s", container, stats.CPUPerc, stats.MemPerc, stats.MemUsage, stats.PIDs)
	// 重启计数是 OOM/崩溃循环的重要信号；读取失败时保留核心 stats，不让整个检查失败。
	if restartOutput, err := t.policy.run(ctx, "inspect", "--format", "{{.RestartCount}}", container); err == nil {
		if restartCount := strings.TrimSpace(string(restartOutput)); restartCount != "" {
			data["restart_count"] = restartCount
			summary += fmt.Sprintf(" restarts=%s", restartCount)
		}
	}

	return schema.Observation{Tool: StatsName, Summary: summary, Data: data}, nil
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

type composePSRow struct {
	Names  string `json:"Names"`
	Labels string `json:"Labels"`
}

// discoverComposeContainers 通过 Docker 的 Compose 标签读取当前项目的容器名。
// 仅缓存允许服务的结果，后续工具只能使用静态白名单或这批已发现的名称。
func (p *policy) discoverComposeContainers(ctx context.Context) ([]string, error) {
	if p.composeProject == "" {
		return append([]string(nil), p.names...), nil
	}
	if len(p.composeServices) == 0 {
		return nil, fmt.Errorf("compose project %q has no allowed services configured", p.composeProject)
	}
	output, err := p.run(ctx, "ps", "--all", "--filter", "label=com.docker.compose.project="+p.composeProject, "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("discover Docker Compose project %q: %w: %s", p.composeProject, err, tools.RedactSensitive(strings.TrimSpace(string(output))))
	}

	discovered := make(map[string]struct{})
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row composePSRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("decode Docker Compose discovery row: %w", err)
		}
		labels := parseDockerLabels(row.Labels)
		if labels["com.docker.compose.project"] != p.composeProject {
			continue
		}
		if _, allowed := p.composeServices[labels["com.docker.compose.service"]]; !allowed {
			continue
		}
		name := strings.TrimSpace(row.Names)
		if name != "" {
			discovered[name] = struct{}{}
		}
	}

	names := make([]string, 0, len(discovered))
	p.discovered.mu.Lock()
	for name := range discovered {
		p.discovered.names[name] = struct{}{}
		names = append(names, name)
	}
	p.discovered.mu.Unlock()
	sort.Strings(names)
	return names, nil
}

// parseDockerLabels 解析 Docker --format {{json .}} 输出中的逗号分隔标签。
// Compose 使用的 project/service 标签值不包含逗号，因此可在不执行额外 inspect 的前提下安全过滤。
func parseDockerLabels(raw string) map[string]string {
	labels := make(map[string]string)
	for _, item := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			labels[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return labels
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
