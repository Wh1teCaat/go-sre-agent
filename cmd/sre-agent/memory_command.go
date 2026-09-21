package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	memory "github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
)

// runMemoryCommand 管理固定 memories 根目录中的收录、检索与重建操作，并返回
// 脚本退出码而不直接结束进程。
func runMemoryCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "rebuild":
		return runMemoryRebuildCommand(args[1:], stdout, stderr)
	case "collect":
		return runMemoryCollectCommand(args[1:], stdout, stderr)
	case "search":
		return runMemorySearchCommand(args[1:], stdout, stderr)
	case "invalidate":
		return runMemoryInvalidateCommand(args[1:], stdout, stderr)
	case "correct":
		return runMemoryCorrectCommand(args[1:], stdout, stderr)
	case "delete":
		return runMemoryDeleteCommand(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown memory command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// runMemoryRebuildCommand 从 rollout summaries 重建所有派生索引。
func runMemoryRebuildCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory rebuild", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	overwrite := fs.Bool("overwrite-generated", false, "explicitly replace manually changed generated index files")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected memory rebuild argument %q\n", fs.Arg(0))
		return 2
	}
	store := memory.NewStore(memory.DefaultDir)
	var err error
	if *overwrite {
		err = store.RebuildForce()
	} else {
		err = store.Rebuild()
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "memories rebuilt")
	return 0
}

// runMemoryCollectCommand 为一个已保存的 run 生成或刷新复盘和索引。
func runMemoryCollectCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory collect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	configPath := fs.String("config", "", "config file path")
	runDir := fs.String("run-dir", "", "override run state directory")
	overwrite := fs.Bool("overwrite-generated", false, "explicitly replace manually changed generated memory files")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(stderr, "memory collect requires --run-id <run_id>")
		return 2
	}
	dir, err := resolveRunDir(*configPath, *runDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	state, err := runstore.NewStore(dir).Load(*runID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	state, err = fillLegacyMemoryScope(state, *configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	store := memory.NewStore(memory.DefaultDir)
	var collected bool
	if *overwrite {
		collected, err = store.UpdateForRunForce(state)
	} else {
		collected, err = store.UpdateForRun(state)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !collected {
		fmt.Fprintln(stdout, "run does not meet memory collection criteria")
		return 0
	}
	fmt.Fprintf(stdout, "memory collected: %s\n", state.RunID)
	return 0
}

// runMemorySearchCommand 输出按服务、环境和关键词匹配的有限历史复盘。
func runMemorySearchCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	goal := fs.String("goal", "", "current diagnostic goal or keywords")
	service := fs.String("service", "", "service scope")
	environment := fs.String("environment", "", "environment scope")
	configPath := fs.String("config", "", "config file path used for default scope")
	maxMatches := fs.Int("max-matches", 3, "maximum matching rollout summaries")
	maxBytes := fs.Int("max-bytes", 12288, "maximum returned history bytes")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected memory search argument %q\n", fs.Arg(0))
		return 2
	}
	resolvedService, resolvedEnvironment, err := resolveMemoryScope(*service, *environment, *configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	matches, err := memory.NewStore(memory.DefaultDir).Search(memory.Query{
		Service:     resolvedService,
		Environment: resolvedEnvironment,
		Goal:        *goal,
		MaxMatches:  *maxMatches,
		MaxBytes:    *maxBytes,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	data, err := json.MarshalIndent(matches, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// runMemoryInvalidateCommand 显式排除已经失效的历史复盘。
func runMemoryInvalidateCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory invalidate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	reason := fs.String("reason", "", "why this knowledge is no longer applicable")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runID) == "" || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "memory invalidate requires --run-id <run_id> --reason <reason>")
		return 2
	}
	if err := memory.NewStore(memory.DefaultDir).Invalidate(*runID, *reason); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "memory invalidated: %s\n", *runID)
	return 0
}

// runMemoryCorrectCommand 显式降低或维持复盘的结论强度。
func runMemoryCorrectCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory correct", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	status := fs.String("conclusion-status", "", "identified, suspected, undetermined, or not_recorded")
	note := fs.String("note", "", "correction note")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runID) == "" || strings.TrimSpace(*status) == "" || strings.TrimSpace(*note) == "" {
		fmt.Fprintln(stderr, "memory correct requires --run-id <run_id> --conclusion-status <status> --note <note>")
		return 2
	}
	if err := memory.NewStore(memory.DefaultDir).Correct(*runID, *status, *note); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "memory corrected: %s\n", *runID)
	return 0
}

// runMemoryDeleteCommand 从可检索知识集中逻辑删除一份复盘，并保留其来源 tombstone。
func runMemoryDeleteCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	runID := fs.String("run-id", "", "run id")
	reason := fs.String("reason", "", "why this knowledge should be removed")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*runID) == "" || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "memory delete requires --run-id <run_id> --reason <reason>")
		return 2
	}
	if err := memory.NewStore(memory.DefaultDir).Delete(*runID, *reason); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "memory deleted from retrieval: %s\n", *runID)
	return 0
}

// resolveMemoryScope 使用显式 flag 或配置默认值确定检索范围。
func resolveMemoryScope(service, environment, configPath string) (string, string, error) {
	if strings.TrimSpace(service) != "" && strings.TrimSpace(environment) != "" {
		return service, environment, nil
	}
	config, err := loadAppConfig(configPath)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(service) == "" {
		service = config.Targets.Service
	}
	if strings.TrimSpace(environment) == "" {
		environment = config.Targets.Environment
	}
	return service, environment, nil
}

// fillLegacyMemoryScope 仅为旧 run 的手动收录补足配置范围；不会回写或伪造其 run JSON。
func fillLegacyMemoryScope(state runstore.State, configPath string) (runstore.State, error) {
	if strings.TrimSpace(state.Service) != "" && strings.TrimSpace(state.Environment) != "" {
		return state, nil
	}
	service, environment, err := resolveMemoryScope(state.Service, state.Environment, configPath)
	if err != nil {
		return state, err
	}
	state.Service = service
	state.Environment = environment
	return state, nil
}
