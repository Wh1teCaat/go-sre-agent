package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
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
	case "process":
		return runMemoryProcessCommand(args[1:], stdout, stderr)
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
	configPath := fs.String("config", "", "config file path")
	runDir := fs.String("run-dir", "", "saved run directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "unexpected memory rebuild argument %q\n", fs.Arg(0))
		return 2
	}
	dir := *runDir
	if dir == "" && *configPath == "" {
		dir = ".runs"
	}
	if dir == "" {
		var err error
		dir, err = resolveRunDir(*configPath, "")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	store := memory.NewStore(memory.DefaultDir).WithRunDir(dir)
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
	store := memory.NewStore(memory.DefaultDir).WithRunDir(dir)
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
	dir, err := resolveRunDir(*configPath, "")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	matches, err := memory.NewStore(memory.DefaultDir).WithRunDir(dir).Search(memory.Query{
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

// runMemoryProcessCommand processes derived work with the configured provider.
func runMemoryProcessCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("memory process", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "config file")
	runDir := fs.String("run-dir", "", "saved run directory")
	runID := fs.String("run-id", "", "process one run and its scope")
	limit := fs.Int("limit", 0, "maximum model tasks")
	timeout := fs.Duration("timeout", 0, "timeout for each model request")
	dryRun := fs.Bool("dry-run", false, "report pending tasks without model calls")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected memory process argument")
		return 2
	}
	cfg, err := loadAppConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *runDir == "" {
		*runDir = cfg.Paths.RunDir
	}
	if *limit == 0 {
		*limit = cfg.Memory.RoundLimit
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "--limit must be positive")
		return 2
	}
	if *timeout == 0 {
		*timeout = cfg.Memory.Timeout
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "--timeout must be positive")
		return 2
	}
	var client llm.ChatClient
	var provider llm.Config
	if !*dryRun {
		provider, err = loadLLMConfig()
		if err == nil {
			client, err = newChatClient(provider)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	extractModel := cfg.Memory.ExtractModel
	if extractModel == "" {
		extractModel = provider.Model
	}
	consolidationModel := cfg.Memory.ConsolidationModel
	if consolidationModel == "" {
		consolidationModel = provider.Model
	}
	ctx, cancel := commandContext()
	defer cancel()
	stats, err := memory.NewStore(memory.DefaultDir).WithRunDir(*runDir).Process(ctx, client, memory.ProcessOptions{RunDir: *runDir, ExtractModel: extractModel, ConsolidationModel: consolidationModel, Timeout: *timeout, Limit: *limit, RunID: *runID, DryRun: *dryRun})
	fmt.Fprintf(stdout, "success=%d skipped=%d stale=%d failed=%d\n", stats.Success, stats.Skipped, stats.Stale, stats.Failed)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if stats.Failed > 0 {
		return 1
	}
	return 0
}

// processAfterDiagnosis is best effort; a memory model failure does not change diagnosis status.
func processAfterDiagnosis(configPath, runDir, runID string, stderr io.Writer) {
	cfg, err := loadAppConfig(configPath)
	if err != nil || !cfg.Memory.ModelEnabled {
		return
	}
	provider, err := loadLLMConfig()
	if err != nil {
		fmt.Fprintf(stderr, "memory process: %v\n", err)
		return
	}
	client, err := newChatClient(provider)
	if err != nil {
		fmt.Fprintf(stderr, "memory process: %v\n", err)
		return
	}
	extractModel := cfg.Memory.ExtractModel
	if extractModel == "" {
		extractModel = provider.Model
	}
	consolidationModel := cfg.Memory.ConsolidationModel
	if consolidationModel == "" {
		consolidationModel = provider.Model
	}
	if runDir == "" {
		runDir = cfg.Paths.RunDir
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.Memory.RoundLimit)*cfg.Memory.Timeout)
	defer cancel()
	stats, err := memory.NewStore(memory.DefaultDir).WithRunDir(runDir).Process(ctx, client, memory.ProcessOptions{RunDir: runDir, RunID: runID, ExtractModel: extractModel, ConsolidationModel: consolidationModel, Timeout: cfg.Memory.Timeout, Limit: cfg.Memory.RoundLimit})
	if err != nil || stats.Failed > 0 {
		fmt.Fprintf(stderr, "memory process deferred: %d failed; %v\n", stats.Failed, err)
	}
}
