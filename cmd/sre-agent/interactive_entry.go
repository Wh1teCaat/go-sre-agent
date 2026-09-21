package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// runCLI 在默认交互入口和保留的非交互子命令之间分派。交互路径的输入、输出和终端
// 属性均可注入测试；脚本子命令通过返回码交由最外层入口退出。
func runCLI(args []string, input io.Reader, output, errorOutput io.Writer, inputTerminal bool, signals <-chan os.Signal) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		options, showHelp, err := parseInteractiveOptions(args, input, output, signals, errorOutput)
		if err != nil {
			fmt.Fprintf(errorOutput, "交互参数错误：%v\n", err)
			return 2
		}
		if showHelp {
			printUsage(output)
			return 0
		}
		if !inputTerminal {
			fmt.Fprintln(errorOutput, "无子命令时只能在终端进入交互模式；请使用 sre diagnose、resume、status、report、memory、eval 或 llm。")
			return 2
		}
		if options.Signals == nil {
			localSignals := make(chan os.Signal, 2)
			signal.Notify(localSignals, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(localSignals)
			options.Signals = localSignals
		}
		if err := runInteractive(options); err != nil {
			fmt.Fprintf(errorOutput, "交互会话启动失败：%v\n", err)
			return 1
		}
		return 0
	}

	switch args[0] {
	case "diagnose":
		return runDiagnoseCommand(args[1:], output, errorOutput)
	case "eval":
		return runEvalCommand(args[1:], output, errorOutput)
	case "resume":
		return runResumeCommand(args[1:], output, errorOutput)
	case "status":
		return runStatusCommand(args[1:], output, errorOutput)
	case "report":
		return runReportCommand(args[1:], output, errorOutput)
	case "memory":
		return runMemoryCommand(args[1:], output, errorOutput)
	case "llm":
		return runLLMCommand(args[1:], output, errorOutput)
	case "help":
		printUsage(output)
		return 0
	default:
		fmt.Fprintf(errorOutput, "未知子命令：%s\n", args[0])
		printUsage(errorOutput)
		return 2
	}
}

// parseInteractiveOptions 使用 ContinueOnError 解析无子命令启动参数，不能让参数错误
// 直接终止交互测试或宿主进程。
func parseInteractiveOptions(args []string, input io.Reader, output io.Writer, signals <-chan os.Signal, errorOutput io.Writer) (interactiveOptions, bool, error) {
	fs := flag.NewFlagSet("sre", flag.ContinueOnError)
	fs.SetOutput(errorOutput)
	configPath := fs.String("config", "", "configuration file path")
	environment := fs.String("environment", "", "environment label")
	sessionID := fs.String("session-id", "", "existing diagnostic session")
	runDir := fs.String("run-dir", "", "override run state directory")
	sessionDir := fs.String("session-dir", "", "override session state directory")
	mockScenario := fs.String("mock-scenario", "", "use a deterministic local mock scenario for each diagnosis")
	overwriteSessionMemory := fs.Bool("overwrite-session-memory", false, "allow replacing modified generated session memory")
	help := fs.Bool("help", false, "show help")
	fs.BoolVar(help, "h", false, "show help")
	if err := fs.Parse(args); err != nil {
		return interactiveOptions{}, false, err
	}
	if fs.NArg() != 0 {
		return interactiveOptions{}, false, fmt.Errorf("unexpected interactive argument %q", fs.Arg(0))
	}
	return interactiveOptions{
		ConfigPath:             *configPath,
		Environment:            *environment,
		SessionID:              *sessionID,
		RunDir:                 *runDir,
		SessionDir:             *sessionDir,
		MockScenario:           *mockScenario,
		OverwriteSessionMemory: *overwriteSessionMemory,
		Input:                  input,
		Output:                 output,
		ProgressOutput:         errorOutput,
		Signals:                signals,
	}, *help, nil
}
