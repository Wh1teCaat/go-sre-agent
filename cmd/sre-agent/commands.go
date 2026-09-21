package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// runDiagnoseCommand 解析 diagnose 参数并执行新诊断。
// 参数: args 为子命令参数；返回: 无，结果写入 stdout/stderr，失败时退出进程。
func runDiagnoseCommand(args []string) {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	goal := fs.String("goal", "", "diagnostic goal")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	maxSteps := fs.Int("max-steps", 0, "maximum agent steps")
	llmTimeout := fs.Duration("llm-timeout", 0, "LLM request timeout")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	taskTimeout := fs.Duration("task-timeout", 0, "whole diagnostic task timeout")
	runDir := fs.String("run-dir", "", "override run state directory")
	sessionID := fs.String("session-id", "", "continue an existing diagnostic session")
	sessionDir := fs.String("session-dir", "", "override session state directory")
	environment := fs.String("environment", "", "session environment label")
	overwriteSessionMemory := fs.Bool("overwrite-session-memory", false, "allow overwrite of manually changed generated session memory")
	out := fs.String("out", "", "override markdown report file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if *goal == "" {
		fmt.Fprintln(os.Stderr, "--goal is required")
		os.Exit(2)
	}

	// CLI 层只做参数收集和退出码处理。
	ctx, stop := commandContext()
	defer stop()
	result, err := startDiagnosisRun(ctx, diagnoseOptions{
		Goal:                   *goal,
		ConfigPath:             *configPath,
		MaxSteps:               *maxSteps,
		LLMTimeout:             *llmTimeout,
		ToolTimeout:            *toolTimeout,
		TaskTimeout:            *taskTimeout,
		RunDir:                 *runDir,
		SessionID:              *sessionID,
		SessionDir:             *sessionDir,
		Environment:            *environment,
		OverwriteSessionMemory: *overwriteSessionMemory,
	}, *mockScenario)
	if err := saveDiagnosisResult(result, err); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "run_id: %s\n", result.State.RunID)
	if result.State.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session_id: %s\n", result.State.SessionID)
	}

	markdown, err := markdownOutput(*out, result)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(markdown)
}

// runResumeCommand 解析 resume 参数并继续未完成的 run。
// 参数: args 为子命令参数；返回: 无，结果写入 stdout/stderr，失败时退出进程。
func runResumeCommand(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	maxSteps := fs.Int("max-steps", 0, "maximum total agent steps")
	llmTimeout := fs.Duration("llm-timeout", 0, "LLM request timeout")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	taskTimeout := fs.Duration("task-timeout", 0, "whole diagnostic task timeout")
	resumeRunning := fs.Bool("resume-running", false, "confirm recovery of a run still marked running")
	runDir := fs.String("run-dir", "", "override run state directory")
	sessionDir := fs.String("session-dir", "", "override session state directory")
	environment := fs.String("environment", "", "session environment label")
	overwriteSessionMemory := fs.Bool("overwrite-session-memory", false, "allow overwrite of manually changed generated session memory")
	out := fs.String("out", "", "override markdown report file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, stop := commandContext()
	defer stop()
	result, err := resumeDiagnosisRun(ctx, resumeOptions{
		RunID:                  *runID,
		RunDir:                 *runDir,
		SessionDir:             *sessionDir,
		Environment:            *environment,
		OverwriteSessionMemory: *overwriteSessionMemory,
		ConfigPath:             *configPath,
		MaxSteps:               *maxSteps,
		LLMTimeout:             *llmTimeout,
		ToolTimeout:            *toolTimeout,
		TaskTimeout:            *taskTimeout,
		ResumeRunning:          *resumeRunning,
	}, *mockScenario)
	if err := saveDiagnosisResult(result, err); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "run_id: %s\n", result.State.RunID)
	if result.State.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session_id: %s\n", result.State.SessionID)
	}
	markdown, err := markdownOutput(*out, result)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(markdown)
}

// commandContext 返回会被 Ctrl-C 或 SIGTERM 取消的上下文。调用方应在持久化
// 最终 checkpoint 后调用 stop。
func commandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// runStatusCommand 输出指定 run 的当前状态。
// 参数: args 为子命令参数；返回: 无，JSON 写入 stdout，失败时退出进程。
func runStatusCommand(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	runDir := fs.String("run-dir", "", "override run state directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	content, err := readDiagnosisStatus(runOptions{RunID: *runID, ConfigPath: *configPath, RunDir: *runDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(content)
}

// runReportCommand 从已保存的 run 重新输出诊断报告。
// 参数: args 为子命令参数；返回: 无，Markdown 写入 stdout，失败时退出进程。
func runReportCommand(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	runDir := fs.String("run-dir", "", "override run state directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	content, err := renderDiagnosisReport(runOptions{RunID: *runID, ConfigPath: *configPath, RunDir: *runDir})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(content)
}

// runLLMCommand 执行底层 LLM ping 或 chat 连通性命令。
// 参数: args 为 llm 子命令参数；返回: 无，模型响应写入 stdout，失败时退出进程。
func runLLMCommand(args []string) {
	if len(args) < 1 {
		printUsageAndExit()
	}

	switch args[0] {
	case "ping":
		content, err := pingLLM(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(content)
	case "chat":
		fs := flag.NewFlagSet("llm chat", flag.ExitOnError)
		message := fs.String("message", "", "message to send to the configured LLM")
		if err := fs.Parse(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if strings.TrimSpace(*message) == "" {
			fmt.Fprintln(os.Stderr, "--message is required")
			os.Exit(2)
		}
		content, err := chatWithLLM(context.Background(), *message)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(content)
	default:
		printUsageAndExit()
	}
}

// markdownOutput 按需保存 Markdown，并始终返回终端输出内容。
// 参数: out 为 CLI 路径，result 为诊断结果；返回: Markdown 内容或写入错误。
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
