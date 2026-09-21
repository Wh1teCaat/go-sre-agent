package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const ProbeName = "docker_probe"

// probeTemplates 是容器内只读探测的固定命令模板。
// 命令完全由模板决定，模型只能选择模板名、容器和端口，不能注入任意命令。
// 这是宿主机无法直连容器网络（如 WSL2）时探测容器内端点的唯一通路。
var probeTemplates = map[string]struct {
	description string
	needsPort   bool
	build       func(port int) []string
}{
	"http_health": {
		description: "GET http://localhost:<port>/health inside the container (for services whose port is not published to the host).",
		needsPort:   true,
		build: func(port int) []string {
			return []string{"sh", "-c", fmt.Sprintf("wget -q -O- -T 4 http://localhost:%d/health", port)}
		},
	},
	"nginx_config": {
		description: "Dump the running nginx config (nginx -T) to reveal the actual resolved upstream servers.",
		build:       func(int) []string { return []string{"nginx", "-T"} },
	},
	"pg_ready": {
		description: "Run pg_isready inside the PostgreSQL container.",
		build:       func(int) []string { return []string{"pg_isready", "-U", "postgres"} },
	},
	"pg_identity": {
		description: "Query current_database() and version() inside the PostgreSQL container to fingerprint the instance.",
		build: func(int) []string {
			return []string{"psql", "-U", "postgres", "-Atc", "select current_database() || ' | ' || version()"}
		},
	},
}

type ProbeArgs struct {
	Probe     string `json:"probe"`
	Container string `json:"container"`
	Port      int    `json:"port,omitempty"`
}

type ProbeTool struct{ policy *policy }

func NewProbe(allowedContainers []string) *ProbeTool {
	return &ProbeTool{policy: newPolicy(allowedContainers, runDocker)}
}

func (t *ProbeTool) Spec() tools.ToolSpec {
	names := make([]string, 0, len(probeTemplates))
	for name := range probeTemplates {
		names = append(names, name)
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("%s: %s", name, probeTemplates[name].description))
	}
	return tools.ToolSpec{
		Name: ProbeName,
		Description: "Run one fixed read-only probe template inside an allowed container via docker exec. Use when the target endpoint is only reachable on the container network. Templates: " +
			strings.Join(lines, " "),
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"probe":     {Type: "string", Required: true, Description: "Probe template name: " + strings.Join(names, ", ")},
				"container": {Type: "string", Required: true, Description: "Exact configured container name."},
				"port":      {Type: "number", Description: "Container-internal port for http_health, for example 8081."},
			},
		},
	}
}

func (t *ProbeTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args ProbeArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode docker_probe args: %w", err)
	}
	template, ok := probeTemplates[strings.TrimSpace(args.Probe)]
	if !ok {
		return schema.Observation{}, fmt.Errorf("unknown probe template %q", args.Probe)
	}
	container, err := t.policy.validate(args.Container)
	if err != nil {
		return schema.Observation{}, err
	}
	if template.needsPort && (args.Port < 1 || args.Port > 65535) {
		return schema.Observation{}, fmt.Errorf("probe %q requires port between 1 and 65535", args.Probe)
	}

	command := append([]string{"exec", container}, template.build(args.Port)...)
	output, err := t.policy.run(ctx, command...)
	text := tools.RedactSensitive(strings.TrimSpace(string(output)))
	if err != nil {
		// 探测失败本身是证据（如容器内 /health 返回 503、pg_isready 拒绝），
		// 保留输出并以 observation 形式回传。
		return schema.Observation{
			Tool:    ProbeName,
			Summary: fmt.Sprintf("probe %s in %s failed", args.Probe, container),
			Data: map[string]any{
				"probe":     args.Probe,
				"container": container,
				"output":    text,
			},
		}, fmt.Errorf("docker exec %s in %q: %w: %s", args.Probe, container, err, text)
	}

	summaryText := text
	if runes := []rune(summaryText); len(runes) > 200 {
		summaryText = string(runes[:200]) + "..."
	}
	summaryText = strings.Join(strings.Fields(summaryText), " ")
	return schema.Observation{
		Tool:    ProbeName,
		Summary: fmt.Sprintf("probe %s in %s: %s", args.Probe, container, summaryText),
		Data: map[string]any{
			"probe":     args.Probe,
			"container": container,
			"output":    text,
		},
	}, nil
}
