package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	memory "github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	sessionstore "github.com/y2/go-sre-agent/internal/session"
	"github.com/y2/go-sre-agent/internal/trace"
)

// TestRunCLIDefaultInteractiveAndNonTerminalProtection 验证默认入口进入交互，而管道输入不会挂起。
func TestRunCLIDefaultInteractiveAndNonTerminalProtection(t *testing.T) {
	configPath := writeTestConfig(t, "")
	var interactiveOutput bytes.Buffer
	if code := runCLI([]string{"--config", configPath, "--mock-scenario", "skeleton"}, strings.NewReader("/exit\n"), &interactiveOutput, &interactiveOutput, true, nil); code != 0 {
		t.Fatalf("interactive exit code = %d, output=%s", code, interactiveOutput.String())
	}
	if !strings.Contains(interactiveOutput.String(), "SRE Agent") || !strings.Contains(interactiveOutput.String(), "会话：") {
		t.Fatalf("interactive banner = %q", interactiveOutput.String())
	}

	var nonTerminalOutput bytes.Buffer
	if code := runCLI(nil, strings.NewReader(""), &nonTerminalOutput, &nonTerminalOutput, false, nil); code != 2 {
		t.Fatalf("non-terminal default code = %d, output=%s", code, nonTerminalOutput.String())
	}
	if !strings.Contains(nonTerminalOutput.String(), "无子命令") {
		t.Fatalf("non-terminal guidance = %q", nonTerminalOutput.String())
	}

	var unknownOutput bytes.Buffer
	if code := runCLI([]string{"unknown"}, strings.NewReader(""), &unknownOutput, &unknownOutput, true, nil); code != 2 {
		t.Fatalf("unknown command code = %d", code)
	}
	if !strings.Contains(unknownOutput.String(), "未知子命令") {
		t.Fatalf("unknown command output = %q", unknownOutput.String())
	}
	if code := runCLI([]string{"--help"}, strings.NewReader(""), &unknownOutput, &unknownOutput, false, nil); code != 0 {
		t.Fatalf("help code = %d", code)
	}
}

// TestIsTerminalRejectsNonTTYCharacterDevice 验证空设备虽可能是字符设备，也不会误入交互读取循环。
func TestIsTerminalRejectsNonTTYCharacterDevice(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open null device: %v", err)
	}
	defer file.Close()
	if isTerminal(file) {
		t.Fatal("null device was treated as an interactive terminal")
	}
}

// TestInteractiveTwoDiagnosesShareSessionAndUseDefaultRun 验证普通文本连续创建不同 run 并默认定位最近 run。
func TestInteractiveTwoDiagnosesShareSessionAndUseDefaultRun(t *testing.T) {
	runDir := t.TempDir()
	sessionDir := t.TempDir()
	var output bytes.Buffer
	cli, err := newInteractiveCLI(interactiveOptions{
		ConfigPath:   writeTestConfig(t, ""),
		RunDir:       runDir,
		SessionDir:   sessionDir,
		MemoryDir:    t.TempDir(),
		MockScenario: "skeleton",
		Input:        strings.NewReader("检查登录接口\n复查登录接口\n/runs\n/sessions\n/status\n/report\n/exit\n"),
		Output:       &output,
	}, interactiveDependencies{})
	if err != nil {
		t.Fatalf("new interactive cli: %v", err)
	}
	firstSession := cli.currentSession
	if err := cli.loop(); err != nil {
		t.Fatalf("interactive loop: %v", err)
	}
	state, err := cli.sessionStore.Load(firstSession)
	if err != nil {
		t.Fatalf("load interactive session: %v", err)
	}
	if len(state.RunIDs) != 2 || state.RunIDs[0] == state.RunIDs[1] || cli.latestRun != state.RunIDs[1] {
		t.Fatalf("interactive session runs = %#v, latest=%q", state.RunIDs, cli.latestRun)
	}
	if !strings.Contains(output.String(), "会话 "+firstSession+" 的运行：") || !strings.Contains(output.String(), "* "+firstSession) || !strings.Contains(output.String(), `"run_id": "`+state.RunIDs[1]) || !strings.Contains(output.String(), "# SRE Diagnosis Report") {
		t.Fatalf("default status/report output = %s", output.String())
	}
}

// TestRunCLIScriptSubcommandsRemainAvailable 验证重构后的脚本子命令返回退出码而不终止测试进程。
func TestRunCLIScriptSubcommandsRemainAvailable(t *testing.T) {
	runDir := t.TempDir()
	state := interactiveMemoryRun("run_script", "session_script", "检查 Redis", time.Unix(60, 0).UTC())
	if err := runstore.NewStore(runDir).Save(state); err != nil {
		t.Fatalf("save script run: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := runCLI([]string{"status", "--run-id", state.RunID, "--run-dir", runDir}, strings.NewReader(""), &stdout, &stderr, false, nil); code != 0 || !strings.Contains(stdout.String(), `"run_id": "run_script"`) {
		t.Fatalf("status code/output = %d / %s / %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCLI([]string{"report", "--run-id", state.RunID, "--run-dir", runDir}, strings.NewReader(""), &stdout, &stderr, false, nil); code != 0 || !strings.Contains(stdout.String(), "# SRE Diagnosis Report") {
		t.Fatalf("report code/output = %d / %s / %s", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := runCLI([]string{"diagnose"}, strings.NewReader(""), &stdout, &stderr, false, nil); code != 2 || !strings.Contains(stderr.String(), "--goal is required") {
		t.Fatalf("diagnose parameter error = %d / %s", code, stderr.String())
	}
	if code := runCLI([]string{"help"}, strings.NewReader(""), &stdout, &stderr, false, nil); code != 0 {
		t.Fatalf("subsequent help code = %d", code)
	}
}

// TestInteractiveSessionSwitchKeepsEnvironmentBoundary 验证 /new 和 /use 不删除历史且拒绝跨环境会话。
func TestInteractiveSessionSwitchKeepsEnvironmentBoundary(t *testing.T) {
	runDir := t.TempDir()
	sessionDir := t.TempDir()
	var output bytes.Buffer
	cli, err := newInteractiveCLI(interactiveOptions{
		ConfigPath:   writeTestConfig(t, ""),
		RunDir:       runDir,
		SessionDir:   sessionDir,
		MemoryDir:    t.TempDir(),
		MockScenario: "skeleton",
		Output:       &output,
	}, interactiveDependencies{})
	if err != nil {
		t.Fatalf("new interactive cli: %v", err)
	}
	cli.handleLine("第一条诊断")
	firstSession := cli.currentSession
	cli.handleLine("/new")
	secondSession := cli.currentSession
	if firstSession == secondSession || cli.newSession {
		t.Fatalf("new session state = %q / %q / %v", firstSession, secondSession, cli.newSession)
	}
	if state, err := cli.sessionStore.Load(secondSession); err != nil || len(state.RunIDs) != 0 {
		t.Fatalf("new session was not created immediately: %#v / %v", state, err)
	}
	cli.handleLine("第二条诊断")
	cli.handleLine("/use " + firstSession)
	if cli.currentSession != firstSession || cli.newSession {
		t.Fatalf("use session state = %q / %v", cli.currentSession, cli.newSession)
	}
	if state, err := cli.sessionStore.Load(secondSession); err != nil || state.Goal != "第二条诊断" || len(state.RunIDs) != 1 {
		t.Fatalf("new session was not updated after diagnosis: %#v / %v", state, err)
	}

	other := "session_other_environment"
	if err := cli.sessionStore.Save(sessionStateForInteractiveTest(other, "other")); err != nil {
		t.Fatalf("save other environment session: %v", err)
	}
	cli.handleLine("/use " + other)
	if cli.currentSession != firstSession || !strings.Contains(output.String(), "不能切换") {
		t.Fatalf("cross-environment switch = %q, output=%s", cli.currentSession, output.String())
	}
}

// TestInteractiveAutomaticMemoryPreviewAndCollection 验证普通诊断自动查询历史并收录新 run，无需 memory 命令。
func TestInteractiveAutomaticMemoryPreviewAndCollection(t *testing.T) {
	runDir := t.TempDir()
	sessionDir := t.TempDir()
	memoryDir := t.TempDir()
	store := memory.NewStore(memoryDir)
	historical := interactiveMemoryRun("run_history", "session_history", "检查 Redis 连接", time.Unix(10, 0).UTC())
	if _, err := store.UpdateForRun(historical); err != nil {
		t.Fatalf("collect historical memory: %v", err)
	}
	var output bytes.Buffer
	cli, err := newInteractiveCLI(interactiveOptions{
		ConfigPath: writeTestConfig(t, ""),
		RunDir:     runDir,
		SessionDir: sessionDir,
		MemoryDir:  memoryDir,
		Output:     &output,
	}, interactiveDependencies{
		startDiagnosis: func(_ context.Context, opts diagnoseOptions, _ string) (diagnoseResult, error) {
			state := interactiveMemoryRun("run_current", opts.SessionID, opts.Goal, time.Unix(20, 0).UTC())
			state.Service = opts.Service
			state.Environment = opts.Environment
			return diagnoseResult{State: state, RunDir: opts.RunDir, SessionDir: opts.SessionDir, Service: opts.Service, MemoryDir: opts.MemoryDir, Environment: opts.Environment, OverwriteSessionMemory: opts.OverwriteSessionMemory}, nil
		},
	})
	if err != nil {
		t.Fatalf("new interactive cli: %v", err)
	}
	cli.handleLine("排查 Redis 连接异常")
	if !strings.Contains(output.String(), "历史记忆：找到 1 条相关记录") {
		t.Fatalf("history preview = %s", output.String())
	}
	matches, err := store.Search(memory.Query{Service: "go-chat", Environment: "local", Goal: "Redis", MaxMatches: 3, MaxBytes: 4096})
	if err != nil || len(matches) != 2 {
		t.Fatalf("automatic memory collection = %#v / %v", matches, err)
	}
	if matches[0].RunID != "run_current" && matches[1].RunID != "run_current" {
		t.Fatalf("current run was not collected: %#v", matches)
	}
}

// TestInteractiveCancellationAllowsNextDiagnosis 验证任务 Ctrl-C 会持久化取消状态，且下一个任务使用新的 context。
func TestInteractiveCancellationAllowsNextDiagnosis(t *testing.T) {
	signals := make(chan os.Signal, 1)
	started := make(chan struct{})
	var output bytes.Buffer
	calls := 0
	cli, err := newInteractiveCLI(interactiveOptions{
		ConfigPath: writeTestConfig(t, ""),
		RunDir:     t.TempDir(),
		SessionDir: t.TempDir(),
		MemoryDir:  t.TempDir(),
		Output:     &output,
		Signals:    signals,
	}, interactiveDependencies{
		startDiagnosis: func(ctx context.Context, opts diagnoseOptions, _ string) (diagnoseResult, error) {
			calls++
			if calls == 1 {
				close(started)
				<-ctx.Done()
				state := interactiveMemoryRun("run_cancelled", opts.SessionID, opts.Goal, time.Unix(30, 0).UTC())
				state.Status = runstore.StatusCancelled
				return diagnoseResult{State: state, RunDir: opts.RunDir, SessionDir: opts.SessionDir, MemoryDir: opts.MemoryDir, Environment: opts.Environment}, ctx.Err()
			}
			state := interactiveMemoryRun("run_after_cancel", opts.SessionID, opts.Goal, time.Unix(40, 0).UTC())
			return diagnoseResult{State: state, RunDir: opts.RunDir, SessionDir: opts.SessionDir, MemoryDir: opts.MemoryDir, Environment: opts.Environment}, nil
		},
	})
	if err != nil {
		t.Fatalf("new interactive cli: %v", err)
	}
	go func() {
		<-started
		signals <- os.Interrupt
	}()
	cli.handleLine("取消中的诊断")
	cli.handleLine("取消后的诊断")
	if calls != 2 || cli.latestRun != "run_after_cancel" {
		t.Fatalf("calls/latest = %d/%q", calls, cli.latestRun)
	}
	if _, err := cli.runStore.Load("run_cancelled"); err != nil {
		t.Fatalf("cancelled run was not persisted: %v", err)
	}
	if !strings.Contains(output.String(), "任务已取消") {
		t.Fatalf("cancellation output = %s", output.String())
	}
}

// TestInteractiveConfirmationAndInputSafety 验证真实模型评测取消不调用模型、删除要确认、引号解析和控制字符清理。
func TestInteractiveConfirmationAndInputSafety(t *testing.T) {
	var output bytes.Buffer
	calledModel := false
	cli, err := newInteractiveCLI(interactiveOptions{
		ConfigPath: writeTestConfig(t, ""),
		RunDir:     t.TempDir(),
		SessionDir: t.TempDir(),
		MemoryDir:  t.TempDir(),
		Output:     &output,
	}, interactiveDependencies{
		modelEvaluation: func(context.Context, modelEvaluationOptions) (storedEvaluation, error) {
			calledModel = true
			return storedEvaluation{}, nil
		},
	})
	if err != nil {
		t.Fatalf("new interactive cli: %v", err)
	}
	cli.handleLine("/eval model login-500")
	cli.handleLine("no")
	if calledModel || !strings.Contains(output.String(), "未授权真实模型评测") {
		t.Fatalf("model confirmation = called:%v output:%s", calledModel, output.String())
	}

	state := interactiveMemoryRun("run_delete", "session_delete", "检查 Redis", time.Unix(50, 0).UTC())
	if _, err := cli.memoryStore.UpdateForRun(state); err != nil {
		t.Fatalf("collect memory to delete: %v", err)
	}
	cli.handleLine("/memory delete run_delete")
	cli.handleLine("过期")
	cli.handleLine("yes")
	matches, err := cli.memoryStore.Search(memory.Query{Service: "go-chat", Environment: "local", Goal: "Redis"})
	if err != nil || len(matches) != 0 {
		t.Fatalf("deleted memory matches = %#v / %v", matches, err)
	}
	arguments, err := splitInteractiveCommand(`/llm chat "hello world"`)
	if err != nil || len(arguments) != 3 || arguments[2] != "hello world" {
		t.Fatalf("quoted command = %#v / %v", arguments, err)
	}
	if got := terminalText("bad\x1b[31m token=secret\n"); strings.Contains(got, "\x1b") || strings.Contains(got, "secret") {
		t.Fatalf("unsafe terminal output = %q", got)
	}
	cli.handleLine("/not-real")
	if !strings.Contains(output.String(), "未知交互命令") {
		t.Fatalf("unknown interactive command = %s", output.String())
	}
}

// interactiveMemoryRun 构造可由 session 和跨会话 memory 存储接受的最小终态运行记录。
func interactiveMemoryRun(runID, sessionID, goal string, updatedAt time.Time) runstore.State {
	return runstore.State{
		RunID:       runID,
		SessionID:   sessionID,
		Service:     "go-chat",
		Environment: "local",
		Goal:        goal,
		Status:      runstore.StatusCompleted,
		Trace: []trace.Entry{{
			Step:     1,
			CallID:   "call_" + runID,
			ToolName: "redis_ping",
			Result:   schema.Observation{Tool: "redis_ping", Summary: "Redis 返回 PONG"},
		}},
		Diagnosis: &schema.Diagnosis{
			Summary:   "Redis 检查已完成。",
			RootCause: &schema.RootCause{Status: "undetermined", Statement: "仍需使用当前证据确认实例。"},
			Evidence:  []schema.Evidence{{Step: 1, Tool: "redis_ping", Summary: "Redis 返回 PONG"}},
		},
		CreatedAt: updatedAt.Add(-time.Second),
		UpdatedAt: updatedAt,
	}
}

// sessionStateForInteractiveTest 构造可用于验证 /use 环境隔离的已持久化会话。
func sessionStateForInteractiveTest(sessionID, environment string) sessionstore.State {
	return sessionstore.State{
		SessionID:   sessionID,
		Goal:        "测试会话",
		Environment: environment,
		CreatedAt:   time.Unix(1, 0).UTC(),
		UpdatedAt:   time.Unix(2, 0).UTC(),
	}
}

// TestInteractiveParserRejectsUnclosedQuotes 验证本地解析器不会将不完整命令交给模型或 shell。
func TestInteractiveParserRejectsUnclosedQuotes(t *testing.T) {
	_, err := splitInteractiveCommand(`/llm chat "missing`)
	if err == nil || !strings.Contains(err.Error(), "没有闭合") {
		t.Fatalf("unclosed quote error = %v", err)
	}
}

// TestInteractiveProgressUsesSeparateSafeStream 验证交互进度使用注入的 stderr，且仍经过终端脱敏边界。
func TestInteractiveProgressUsesSeparateSafeStream(t *testing.T) {
	var normalOutput bytes.Buffer
	var progressOutput bytes.Buffer
	cli, err := newInteractiveCLI(interactiveOptions{
		ConfigPath:     writeTestConfig(t, ""),
		RunDir:         t.TempDir(),
		SessionDir:     t.TempDir(),
		MemoryDir:      t.TempDir(),
		Output:         &normalOutput,
		ProgressOutput: &progressOutput,
	}, interactiveDependencies{})
	if err != nil {
		t.Fatalf("new interactive cli: %v", err)
	}
	cli.progress.Report(agent.ProgressEvent{
		Kind:     agent.ProgressCheckCompleted,
		Step:     1,
		Summary:  "token=secret" + string(rune(27)) + "[31m",
		Duration: time.Millisecond,
	})
	if normalOutput.Len() != 0 {
		t.Fatalf("normal output unexpectedly received progress: %q", normalOutput.String())
	}
	if got := progressOutput.String(); strings.Contains(got, "secret") || strings.ContainsRune(got, rune(27)) || !strings.Contains(got, "检查完成") {
		t.Fatalf("unsafe progress output = %q", got)
	}
}
