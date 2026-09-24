package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	if code := runCLI(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, isTerminal(os.Stdin), nil); code != 0 {
		os.Exit(code)
	}
}

// printUsage 输出可脚本调用的 sre 子命令和默认交互入口说明。
func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage: sre [--config config.yaml] [--environment local] [--session-id <session_id>] [--run-dir .runs] [--session-dir .sessions] [--plain]")
	fmt.Fprintln(writer, "       sre diagnose --goal <goal> [--task-timeout 5m] [--session-id <session_id>] [--session-dir .sessions] [--environment local] [--overwrite-session-memory] [--config config.yaml] [--mock-scenario login-500] [--out report.md]")
	fmt.Fprintln(writer, "       sre eval mock [--scenario all] [--results-dir evals/results]")
	fmt.Fprintln(writer, "       sre eval model [--scenario login-500] [--config config.yaml] [--results-dir evals/results] [--execute-real-model]")
	fmt.Fprintln(writer, "       sre resume --run-id <run_id> [--task-timeout 5m] [--resume-running] [--session-dir .sessions] [--environment local] [--overwrite-session-memory] [--config config.yaml]")
	fmt.Fprintln(writer, "       sre status --run-id <run_id> [--config config.yaml]")
	fmt.Fprintln(writer, "       sre report --run-id <run_id> [--config config.yaml]")
	fmt.Fprintln(writer, "       sre memory process [--limit 2] [--timeout 30s] [--run-id <run_id>] [--dry-run] [--config config.yaml] [--run-dir .runs]")
	fmt.Fprintln(writer, "       sre memory rebuild [--overwrite-generated] [--config config.yaml] [--run-dir .runs]")
	fmt.Fprintln(writer, "       sre memory collect --run-id <run_id> [--run-dir .runs] [--config config.yaml] [--overwrite-generated]")
	fmt.Fprintln(writer, "       sre memory search --goal <goal> [--service go-chat] [--environment local]")
	fmt.Fprintln(writer, "       sre memory invalidate --run-id <run_id> --reason <reason>")
	fmt.Fprintln(writer, "       sre memory correct --run-id <run_id> --conclusion-status <status> --note <note>")
	fmt.Fprintln(writer, "       sre memory delete --run-id <run_id> --reason <reason>")
	fmt.Fprintln(writer, "       sre llm ping")
	fmt.Fprintln(writer, "       sre llm chat --message <message>")
}
