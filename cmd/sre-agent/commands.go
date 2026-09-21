package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	memory "github.com/y2/go-sre-agent/internal/memory"
)

// runDiagnoseCommand 解析脚本 diagnose 参数并复用诊断应用函数。它不退出进程，
// 因而最外层入口和交互入口可以采用各自合适的错误处理方式。
func runDiagnoseCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	goal := fs.String("goal", "", "diagnostic goal")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	maxSteps := fs.Int("max-steps", 0, "maximum agent steps")
	llmTimeout := fs.Duration("llm-timeout", 0, "LLM request timeout")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	taskTimeout := fs.Duration("task-timeout", 0, "whole diagnostic task timeout")
	maxToolCalls := fs.Int("max-tool-calls", 0, "maximum tool calls in one diagnostic run")
	maxParallelTools := fs.Int("max-parallel-tools", 0, "maximum concurrent independent read-only tool calls")
	contextBudgetBytes := fs.Int("context-budget-bytes", 0, "maximum observation-context bytes sent to the model")
	toolOutputBudgetBytes := fs.Int("tool-output-budget-bytes", 0, "maximum bytes from one tool observation sent to the model")
	runDir := fs.String("run-dir", "", "override run state directory")
	sessionID := fs.String("session-id", "", "continue an existing diagnostic session")
	sessionDir := fs.String("session-dir", "", "override session state directory")
	environment := fs.String("environment", "", "session environment label")
	overwriteSessionMemory := fs.Bool("overwrite-session-memory", false, "allow overwrite of manually changed generated session memory")
	out := fs.String("out", "", "override markdown report file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected diagnose argument %q\n", fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(*goal) == "" {
		fmt.Fprintln(stderr, "--goal is required")
		return 2
	}

	ctx, stop := commandContext()
	defer stop()
	progress := newCLIProgressWriter(stderr)
	result, err := startDiagnosisRun(ctx, diagnoseOptions{
		Goal:                   *goal,
		ConfigPath:             *configPath,
		MaxSteps:               *maxSteps,
		LLMTimeout:             *llmTimeout,
		ToolTimeout:            *toolTimeout,
		TaskTimeout:            *taskTimeout,
		MaxToolCalls:           *maxToolCalls,
		MaxParallelTools:       *maxParallelTools,
		ContextBudgetBytes:     *contextBudgetBytes,
		ToolOutputBudgetBytes:  *toolOutputBudgetBytes,
		Progress:               progress.Report,
		RunDir:                 *runDir,
		SessionID:              *sessionID,
		SessionDir:             *sessionDir,
		MemoryDir:              memory.DefaultDir,
		Environment:            *environment,
		OverwriteSessionMemory: *overwriteSessionMemory,
	}, *mockScenario)
	if err := saveDiagnosisResult(result, err); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stderr, "run_id: %s\n", result.State.RunID)
	if result.State.SessionID != "" {
		fmt.Fprintf(stderr, "session_id: %s\n", result.State.SessionID)
	}
	markdown, err := markdownOutput(*out, result)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *out != "" || result.ReportDir != "" {
		fmt.Fprintln(stderr, "诊断完成，报告已保存。")
	} else {
		fmt.Fprintln(stderr, "诊断完成。")
	}
	fmt.Fprint(stdout, markdown)
	return 0
}

// runResumeCommand 解析脚本 resume 参数，并复用受既有安全限制保护的恢复路径。
func runResumeCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	maxSteps := fs.Int("max-steps", 0, "maximum total agent steps")
	llmTimeout := fs.Duration("llm-timeout", 0, "LLM request timeout")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	taskTimeout := fs.Duration("task-timeout", 0, "whole diagnostic task timeout")
	maxToolCalls := fs.Int("max-tool-calls", 0, "maximum tool calls in one diagnostic run")
	maxParallelTools := fs.Int("max-parallel-tools", 0, "maximum concurrent independent read-only tool calls")
	contextBudgetBytes := fs.Int("context-budget-bytes", 0, "maximum observation-context bytes sent to the model")
	toolOutputBudgetBytes := fs.Int("tool-output-budget-bytes", 0, "maximum bytes from one tool observation sent to the model")
	resumeRunning := fs.Bool("resume-running", false, "confirm recovery of a run still marked running")
	runDir := fs.String("run-dir", "", "override run state directory")
	sessionDir := fs.String("session-dir", "", "override session state directory")
	environment := fs.String("environment", "", "session environment label")
	overwriteSessionMemory := fs.Bool("overwrite-session-memory", false, "allow overwrite of manually changed generated session memory")
	out := fs.String("out", "", "override markdown report file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected resume argument %q\n", fs.Arg(0))
		return 2
	}
	if strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "--run-id is required")
		return 2
	}

	ctx, stop := commandContext()
	defer stop()
	progress := newCLIProgressWriter(stderr)
	result, err := resumeDiagnosisRun(ctx, resumeOptions{
		RunID:                  *runID,
		RunDir:                 *runDir,
		SessionDir:             *sessionDir,
		MemoryDir:              memory.DefaultDir,
		Environment:            *environment,
		OverwriteSessionMemory: *overwriteSessionMemory,
		ConfigPath:             *configPath,
		MaxSteps:               *maxSteps,
		LLMTimeout:             *llmTimeout,
		ToolTimeout:            *toolTimeout,
		TaskTimeout:            *taskTimeout,
		MaxToolCalls:           *maxToolCalls,
		MaxParallelTools:       *maxParallelTools,
		ContextBudgetBytes:     *contextBudgetBytes,
		ToolOutputBudgetBytes:  *toolOutputBudgetBytes,
		Progress:               progress.Report,
		ResumeRunning:          *resumeRunning,
	}, *mockScenario)
	if err := saveDiagnosisResult(result, err); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stderr, "run_id: %s\n", result.State.RunID)
	if result.State.SessionID != "" {
		fmt.Fprintf(stderr, "session_id: %s\n", result.State.SessionID)
	}
	markdown, err := markdownOutput(*out, result)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *out != "" || result.ReportDir != "" {
		fmt.Fprintln(stderr, "诊断完成，报告已保存。")
	} else {
		fmt.Fprintln(stderr, "诊断完成。")
	}
	fmt.Fprint(stdout, markdown)
	return 0
}

// commandContext 返回会被 Ctrl-C 或 SIGTERM 取消的脚本命令上下文。
func commandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// runStatusCommand 输出指定 run 的机器可读状态。
func runStatusCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	runDir := fs.String("run-dir", "", "override run state directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "status requires --run-id <run_id>")
		return 2
	}
	content, err := readDiagnosisStatus(runOptions{RunID: *runID, ConfigPath: *configPath, RunDir: *runDir})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, content)
	return 0
}

// runReportCommand 从已保存运行渲染 Markdown 报告。
func runReportCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	runDir := fs.String("run-dir", "", "override run state directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "report requires --run-id <run_id>")
		return 2
	}
	content, err := renderDiagnosisReport(runOptions{RunID: *runID, ConfigPath: *configPath, RunDir: *runDir})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprint(stdout, content)
	return 0
}

// runLLMCommand 执行直接模型调试，不创建诊断 run。
func runLLMCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "ping":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: sre llm ping")
			return 2
		}
		content, err := pingLLM(context.Background())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, content)
		return 0
	case "chat":
		fs := flag.NewFlagSet("llm chat", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		message := fs.String("message", "", "message to send to the configured LLM")
		if err := fs.Parse(args[1:]); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if fs.NArg() != 0 || strings.TrimSpace(*message) == "" {
			fmt.Fprintln(stderr, "llm chat requires --message <message>")
			return 2
		}
		content, err := chatWithLLM(context.Background(), *message)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, content)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown llm command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// markdownOutput 按需保存 Markdown，并始终返回终端输出内容。
func markdownOutput(out string, result diagnoseResult) (string, error) {
	reportPath := strings.TrimSpace(out)
	if reportPath == "" && strings.TrimSpace(result.ReportDir) != "" {
		reportPath = filepath.Join(result.ReportDir, result.State.RunID+".md")
	}
	if reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o755); err != nil {
			return "", fmt.Errorf("create report dir: %w", err)
		}
		if err := os.WriteFile(reportPath, []byte(result.Markdown), 0o600); err != nil {
			return "", fmt.Errorf("write report: %w", err)
		}
		if err := os.Chmod(reportPath, 0o600); err != nil {
			return "", fmt.Errorf("secure report: %w", err)
		}
	}
	return result.Markdown, nil
}
