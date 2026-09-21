// smoke 包将被诊断项目自带的端到端冒烟脚本包装成合成事务探针。
//
// 这是工具集中唯一的非只读探测：冒烟脚本会以测试账号执行真实业务写入
// （注册/登录/发消息）。因此它不默认存在——只有运营者在配置中显式写出
// smoke_command 时才会注册，命令内容完全来自配置，模型无法指定或改写。
package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const Name = "smoke_run"

// maxOutputBytes 截断脚本输出，防止异常脚本把 observation 塞爆。
const maxOutputBytes = 64 << 10

type Tool struct {
	command []string
	dir     string
	timeout time.Duration
}

// New 构造合成事务工具。command 为完整命令行（如 ["go","run","./smoketest"]），
// dir 为工作目录；两者均来自配置。
func New(command []string, dir string, timeout time.Duration) *Tool {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Tool{command: command, dir: dir, timeout: timeout}
}

func (t *Tool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name: Name,
		Description: "Run the operator-configured end-to-end smoke test as a synthetic transaction (register/login/send message/push/pull). " +
			"This performs real writes with a test account. The first FAIL or FATAL assertion name pinpoints the broken stage of the message path; " +
			"silent push-loss faults (for example Kafka publish degradation) are only detectable this way.",
		Schema:     tools.ToolSchema{Properties: map[string]tools.ArgSpec{}},
		SideEffect: true,
		Timeout:    t.timeout,
	}
}

func (t *Tool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	if len(t.command) == 0 {
		return schema.Observation{}, fmt.Errorf("smoke command is not configured")
	}

	startedAt := time.Now()
	command := exec.CommandContext(ctx, t.command[0], t.command[1:]...)
	command.Dir = t.dir
	output, runErr := command.CombinedOutput()
	if len(output) > maxOutputBytes {
		output = output[:maxOutputBytes]
	}
	text := tools.RedactSensitive(string(output))
	durationMS := time.Since(startedAt).Milliseconds()

	passed, failures, result := parseSmokeOutput(text)
	data := map[string]any{
		"passed":      passed,
		"failed":      len(failures),
		"failures":    failures,
		"result":      result,
		"duration_ms": durationMS,
	}

	summary := fmt.Sprintf("smoke test: %d passed, %d failed in %dms", passed, len(failures), durationMS)
	if len(failures) > 0 {
		summary += fmt.Sprintf("; first failure: %s", failures[0])
	}

	if runErr != nil && passed == 0 && len(failures) == 0 {
		// 脚本没跑起来（编译失败、目录不存在等），与业务断言失败区分开。
		return schema.Observation{
			Tool:    Name,
			Summary: "smoke test failed to run",
			Data:    map[string]any{"output": tailLines(text, 10), "duration_ms": durationMS},
		}, fmt.Errorf("run smoke test: %w", runErr)
	}
	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data:    data,
	}, nil
}

// parseSmokeOutput 解析冒烟脚本的行协议：
// "PASS  <name>"、"FAIL  <name> — <detail>"、"FATAL <name> — <err>"、"RESULT: ..."。
func parseSmokeOutput(output string) (passed int, failures []string, result string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "PASS"):
			passed++
		case strings.HasPrefix(line, "FAIL"), strings.HasPrefix(line, "FATAL"):
			failures = append(failures, line)
		case strings.HasPrefix(line, "RESULT:"):
			result = strings.TrimSpace(strings.TrimPrefix(line, "RESULT:"))
		}
	}
	return passed, failures, result
}

func tailLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
