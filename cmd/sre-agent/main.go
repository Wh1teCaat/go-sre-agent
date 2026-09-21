package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		printUsageAndExit()
	}

	switch os.Args[1] {
	case "diagnose":
		runDiagnoseCommand(os.Args[2:])
	case "eval":
		runEvalCommand(os.Args[2:])
	case "resume":
		runResumeCommand(os.Args[2:])
	case "status":
		runStatusCommand(os.Args[2:])
	case "report":
		runReportCommand(os.Args[2:])
	case "memory":
		runMemoryCommand(os.Args[2:])
	case "llm":
		runLLMCommand(os.Args[2:])
	default:
		printUsageAndExit()
	}
}

// printUsageAndExit 输出 CLI 用法并以参数错误状态退出。
// 参数: 无；返回: 无，函数固定以退出码 2 终止进程。
func printUsageAndExit() {
	fmt.Fprintln(os.Stderr, "usage: sre-agent diagnose --goal <goal> [--task-timeout 5m] [--session-id <session_id>] [--session-dir .sessions] [--environment local] [--overwrite-session-memory] [--config config.yaml] [--mock-scenario login-500] [--out report.md]")
	fmt.Fprintln(os.Stderr, "       sre-agent eval mock [--scenario all] [--results-dir evals/results]")
	fmt.Fprintln(os.Stderr, "       sre-agent eval model [--scenario login-500] [--config config.yaml] [--results-dir evals/results] [--execute-real-model]")
	fmt.Fprintln(os.Stderr, "       sre-agent resume --run-id <run_id> [--task-timeout 5m] [--resume-running] [--session-dir .sessions] [--environment local] [--overwrite-session-memory] [--config config.yaml]")
	fmt.Fprintln(os.Stderr, "       sre-agent status --run-id <run_id> [--config config.yaml]")
	fmt.Fprintln(os.Stderr, "       sre-agent report --run-id <run_id> [--config config.yaml]")
	fmt.Fprintln(os.Stderr, "       sre-agent memory rebuild [--overwrite-generated]")
	fmt.Fprintln(os.Stderr, "       sre-agent memory collect --run-id <run_id> [--run-dir .runs] [--config config.yaml] [--overwrite-generated]")
	fmt.Fprintln(os.Stderr, "       sre-agent memory search --goal <goal> [--service go-chat] [--environment local]")
	fmt.Fprintln(os.Stderr, "       sre-agent memory invalidate --run-id <run_id> --reason <reason>")
	fmt.Fprintln(os.Stderr, "       sre-agent memory correct --run-id <run_id> --conclusion-status <status> --note <note>")
	fmt.Fprintln(os.Stderr, "       sre-agent memory delete --run-id <run_id> --reason <reason>")
	fmt.Fprintln(os.Stderr, "       sre-agent llm ping")
	fmt.Fprintln(os.Stderr, "       sre-agent llm chat --message <message>")
	os.Exit(2)
}
