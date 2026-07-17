package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func runDiagnoseCommand(args []string) {
	fs := flag.NewFlagSet("diagnose", flag.ExitOnError)
	goal := fs.String("goal", "", "diagnostic goal")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	maxSteps := fs.Int("max-steps", 0, "maximum agent steps")
	llmTimeout := fs.Duration("llm-timeout", 0, "LLM request timeout")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	runDir := fs.String("run-dir", "", "override run state directory")
	out := fs.String("out", "", "override markdown report file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if *goal == "" {
		fmt.Fprintln(os.Stderr, "--goal is required")
		os.Exit(2)
	}

	// CLI 层只做参数收集和退出码处理；诊断执行和保存分别由
	// startDiagnosisRun/saveDiagnosisRun 处理，避免依赖 os.Args 和 os.Exit。
	result, err := startDiagnosisRun(context.Background(), diagnoseOptions{
		Goal:        *goal,
		ConfigPath:  *configPath,
		MaxSteps:    *maxSteps,
		LLMTimeout:  *llmTimeout,
		ToolTimeout: *toolTimeout,
		RunDir:      *runDir,
	}, *mockScenario)
	if err != nil {
		if saveErr := saveDiagnosisRun(result); saveErr != nil {
			fmt.Fprintf(os.Stderr, "%s\nsave run state: %v\n", runErrorMessage(result, err), saveErr)
		} else {
			fmt.Fprintln(os.Stderr, runErrorMessage(result, err))
		}
		os.Exit(1)
	}
	if err = saveDiagnosisRun(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "run_id: %s\n", result.State.RunID)

	markdown, err := markdownOutput(*out, result)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(markdown)
}

func runResumeCommand(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	mockScenario := fs.String("mock-scenario", "", "mock scenario name")
	maxSteps := fs.Int("max-steps", 0, "maximum total agent steps")
	llmTimeout := fs.Duration("llm-timeout", 0, "LLM request timeout")
	toolTimeout := fs.Duration("tool-timeout", 0, "tool execution timeout")
	runDir := fs.String("run-dir", "", "override run state directory")
	out := fs.String("out", "", "override markdown report file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	result, err := resumeDiagnosisRun(context.Background(), resumeOptions{
		RunID:       *runID,
		RunDir:      *runDir,
		ConfigPath:  *configPath,
		MaxSteps:    *maxSteps,
		LLMTimeout:  *llmTimeout,
		ToolTimeout: *toolTimeout,
	}, *mockScenario)
	if err != nil {
		if saveErr := saveDiagnosisRun(result); saveErr != nil {
			fmt.Fprintf(os.Stderr, "%s\nsave run state: %v\n", runErrorMessage(result, err), saveErr)
		} else {
			fmt.Fprintln(os.Stderr, runErrorMessage(result, err))
		}
		os.Exit(1)
	}
	if err = saveDiagnosisRun(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "run_id: %s\n", result.State.RunID)
	markdown, err := markdownOutput(*out, result)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(markdown)
}

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

func reportOutputPath(out string, reportDir string, runID string) string {
	out = strings.TrimSpace(out)
	if out != "" {
		return out
	}
	reportDir = strings.TrimSpace(reportDir)
	if reportDir == "" {
		return ""
	}
	return filepath.Join(reportDir, runID+".md")
}

func markdownOutput(out string, result diagnoseResult) (string, error) {
	reportPath := reportOutputPath(out, result.ReportDir, result.State.RunID)
	if reportPath != "" {
		if err := writeMarkdownReport(reportPath, result.Markdown); err != nil {
			return "", err
		}
	}
	return result.Markdown, nil
}

func writeMarkdownReport(path string, markdown string) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create report dir: %w", err)
		}
	}
	if err := os.WriteFile(path, []byte(markdown), 0o644); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func runErrorMessage(result diagnoseResult, err error) string {
	if result.State.RunID == "" {
		return err.Error()
	}
	return fmt.Sprintf("run_id: %s\n%s", result.State.RunID, err)
}
