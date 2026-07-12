package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	appconfig "github.com/y2/go-sre-agent/internal/config"
	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/report"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/tools/docker"
	"github.com/y2/go-sre-agent/internal/tools/httpcheck"
	"github.com/y2/go-sre-agent/internal/tools/logread"
	"github.com/y2/go-sre-agent/internal/tools/postgres"
	"github.com/y2/go-sre-agent/internal/tools/redis"
	"github.com/y2/go-sre-agent/internal/tools/websocket"
	"github.com/y2/go-sre-agent/internal/trace"
)

// diagnoseOnce 组装一次诊断所需的 registry、provider、policy、runtime，
// 执行完成后把 diagnosis 和内部 trace 交给报告层生成 Markdown。
func diagnoseOnce(ctx context.Context, opts diagnoseOptions, mockScenario string) (string, error) {
	result, err := startDiagnosisRun(ctx, opts, mockScenario)
	return result.Markdown, err
}

func startDiagnosisRun(ctx context.Context, opts diagnoseOptions, mockScenario string) (diagnoseResult, error) {
	startedAt := time.Now().UTC()
	return executeDiagnosisRun(ctx, opts, mockScenario, runstore.NewRunID(startedAt), startedAt, nil, schema.Plan{})
}

// resumeDiagnosisRun 从已保存的 run state 恢复 trace，并用同一个 run id 更新保存结果。
// 目标和历史 trace 来自 run state；目标环境参数来自配置文件。
func resumeDiagnosisRun(ctx context.Context, opts resumeOptions, mockScenario string) (diagnoseResult, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return diagnoseResult{}, fmt.Errorf("run id is required")
	}
	runDir, err := resolveRunDir(opts.ConfigPath, opts.RunDir)
	if err != nil {
		return diagnoseResult{}, err
	}
	store := runstore.NewStore(runDir)
	previous, err := store.Load(opts.RunID)
	if err != nil {
		return diagnoseResult{}, err
	}
	if previous.Status == runstore.StatusCompleted && previous.Diagnosis != nil {
		return diagnoseResult{}, fmt.Errorf("run %q is already completed; use report instead", opts.RunID)
	}

	result, runErr := executeDiagnosisRun(ctx, diagnoseOptions{
		Goal:        previous.Goal,
		ConfigPath:  opts.ConfigPath,
		MaxSteps:    opts.MaxSteps,
		LLMTimeout:  opts.LLMTimeout,
		ToolTimeout: opts.ToolTimeout,
		RunDir:      runDir,
	}, mockScenario, previous.RunID, previous.CreatedAt, previous.Trace, previous.Plan)
	return result, runErr
}

func executeDiagnosisRun(ctx context.Context, opts diagnoseOptions, mockScenario string, runID string, createdAt time.Time, existingTrace []trace.Entry, existingPlan schema.Plan) (diagnoseResult, error) {
	cfg, err := resolveDiagnosisConfig(opts)
	if err != nil {
		return diagnoseResult{}, err
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	// Registry 是模型可选工具的唯一来源。注册时就注入 host/path 等边界，
	// 后续 runtime 只按工具名查找并执行，不再临时拼装工具实例。
	registry := tools.NewRegistry()
	if err := registry.Register(httpcheck.NewWithPolicy(nil, 2048, cfg.AllowedHosts, []string{loginURLForDiagnose(cfg)})); err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}
	if err := registry.Register(logread.New(cfg.AllowedLogDirs, 1000)); err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}
	if err := registry.Register(postgres.NewPing(nil)); err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}
	if err := registry.Register(postgres.NewCheck(nil)); err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}
	if err := registry.Register(redis.New(nil)); err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}
	if err := registry.Register(websocket.NewWithAllowedHosts(nil, cfg.AllowedHosts)); err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}
	for _, tool := range []tools.Tool{
		docker.NewPS(cfg.AllowedContainers),
		docker.NewInspect(cfg.AllowedContainers),
		docker.NewLogs(cfg.AllowedContainers),
	} {
		if err := registry.Register(tool); err != nil {
			return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
		}
	}

	provider, model, err := buildLLMProvider(cfg, mockScenario)
	if err != nil {
		return diagnoseResult{RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}

	traceStore := trace.NewMemoryStoreWithEntries(existingTrace)
	// Runtime 把 provider、registry、policy 和 trace 串起来：
	// provider 决定下一步，policy 决定能不能执行，registry 执行工具，trace 留证。
	runtime := agent.NewRuntime(agent.RuntimeConfig{
		MaxSteps:         cfg.MaxSteps,
		LLMTimeout:       cfg.LLMTimeout,
		ToolTimeout:      cfg.ToolTimeout,
		Model:            model,
		TargetContext:    targetContextForDiagnose(cfg),
		ToolArgOverrides: toolArgOverridesForDiagnose(cfg),
		Plan:             existingPlan,
		Memories:         memoryHintsForDiagnose(cfg.RunDir, runID),
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      cfg.MaxSteps,
		ToolAllowlist: cfg.ToolAllowlist,
		ToolTimeout:   cfg.ToolTimeout,
		AllowedHosts:  cfg.AllowedHosts,
		ToolSchemas:   toolSchemasFromRegistry(registry),
	}), traceStore)

	diagnosis, err := runtime.Run(ctx, cfg.Goal)
	if err != nil {
		state := runstore.State{
			RunID:     runID,
			Goal:      cfg.Goal,
			Status:    runstore.StatusFailed,
			Plan:      runtime.Plan(),
			Trace:     traceStore.List(),
			Error:     err.Error(),
			CreatedAt: createdAt,
			UpdatedAt: time.Now().UTC(),
		}
		return diagnoseResult{State: state, RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, err
	}

	markdown := report.Markdown(report.Input{
		Goal:      cfg.Goal,
		Diagnosis: *diagnosis,
		Plan:      runtime.Plan(),
		Trace:     traceStore.List(),
	})
	state := runstore.State{
		RunID:     runID,
		Goal:      cfg.Goal,
		Status:    runstore.StatusCompleted,
		Plan:      runtime.Plan(),
		Diagnosis: diagnosis,
		Trace:     traceStore.List(),
		CreatedAt: createdAt,
		UpdatedAt: time.Now().UTC(),
	}
	return diagnoseResult{Markdown: markdown, State: state, RunDir: cfg.RunDir, ReportDir: cfg.ReportDir}, nil
}

// memoryHintsForDiagnose 把历史完成运行压缩成模型可见线索。
// 旧 trace/evidence 不进入当前上下文，避免历史步骤被误当成本次证据。
func memoryHintsForDiagnose(runDir string, currentRunID string) []schema.Memory {
	states := runstore.NewStore(runDir).RecentCompleted(3)
	memories := make([]schema.Memory, 0, len(states))
	for _, state := range states {
		if state.RunID == currentRunID {
			continue
		}
		memories = append(memories, schema.Memory{
			Subject:     tools.RedactSensitive(state.Goal),
			Content:     tools.RedactSensitive(state.Diagnosis.Summary),
			SourceRunID: state.RunID,
		})
	}
	return memories
}

func saveDiagnosisRun(result diagnoseResult) error {
	if result.State.RunID == "" {
		return nil
	}
	if err := runstore.NewStore(result.RunDir).Save(result.State); err != nil {
		return err
	}
	return nil
}

func readDiagnosisStatus(opts statusOptions) (string, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return "", fmt.Errorf("run id is required")
	}
	runDir, err := resolveRunDir(opts.ConfigPath, opts.RunDir)
	if err != nil {
		return "", err
	}
	state, err := runstore.NewStore(runDir).Load(opts.RunID)
	if err != nil {
		return "", err
	}
	view := struct {
		RunID      string          `json:"run_id"`
		Goal       string          `json:"goal"`
		Status     runstore.Status `json:"status"`
		Plan       schema.Plan     `json:"plan"`
		Error      string          `json:"error,omitempty"`
		TraceSteps int             `json:"trace_steps"`
		CreatedAt  time.Time       `json:"created_at"`
		UpdatedAt  time.Time       `json:"updated_at"`
	}{
		RunID:      state.RunID,
		Goal:       state.Goal,
		Status:     state.Status,
		Plan:       state.Plan,
		Error:      state.Error,
		TraceSteps: len(state.Trace),
		CreatedAt:  state.CreatedAt,
		UpdatedAt:  state.UpdatedAt,
	}
	data, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode run status: %w", err)
	}
	return string(data), nil
}

func renderDiagnosisReport(opts reportOptions) (string, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return "", fmt.Errorf("run id is required")
	}
	runDir, err := resolveRunDir(opts.ConfigPath, opts.RunDir)
	if err != nil {
		return "", err
	}
	state, err := runstore.NewStore(runDir).Load(opts.RunID)
	if err != nil {
		return "", err
	}
	if state.Diagnosis == nil {
		return "", fmt.Errorf("run %q has no final diagnosis", opts.RunID)
	}
	return report.Markdown(report.Input{
		Goal:      state.Goal,
		Diagnosis: *state.Diagnosis,
		Plan:      state.Plan,
		Trace:     state.Trace,
	}), nil
}

// targetContextForDiagnose 构造发给 LLM 的目标上下文。
// 这些信息只用于选择工具参数，不写入 trace，也不能作为最终报告证据。
func targetContextForDiagnose(cfg diagnosisConfig) map[string]any {
	return map[string]any{
		"backend_base_url":        cfg.BackendBaseURL,
		"login_url":               loginURLForDiagnose(cfg),
		"postgres_target":         safePostgresDSNForPrompt(cfg.PostgresDSN),
		"postgres_dsn_configured": strings.TrimSpace(cfg.PostgresDSN) != "",
		"redis_addr":              cfg.RedisAddr,
		"websocket_url":           cfg.WebSocketURL,
		"log_file":                cfg.LogFile,
		"allowed_hosts":           cfg.AllowedHosts,
		"docker_containers":       cfg.AllowedContainers,
	}
}

func loginURLForDiagnose(cfg diagnosisConfig) string {
	if strings.TrimSpace(cfg.BackendBaseURL) == "" {
		return ""
	}
	return strings.TrimRight(cfg.BackendBaseURL, "/") + "/v1/user/login"
}

func toolArgOverridesForDiagnose(cfg diagnosisConfig) map[string]map[string]any {
	overrides := map[string]map[string]any{}
	dsn := strings.TrimSpace(cfg.PostgresDSN)
	if dsn != "" {
		overrides[postgres.PingName] = map[string]any{"dsn": dsn}
		overrides[postgres.CheckName] = map[string]any{"dsn": dsn}
	}
	if addr := strings.TrimSpace(cfg.RedisAddr); addr != "" {
		overrides[redis.Name] = map[string]any{"addr": addr}
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}

// safePostgresDSNForPrompt 在 DSN 进入 prompt 前移除密码等敏感信息。
// 工具真正执行时仍使用原始 DSN，模型侧只看到可识别目标但不可泄密的版本。
func safePostgresDSNForPrompt(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			return "[REDACTED_POSTGRES_DSN]"
		}
		if parsed.User != nil {
			username := parsed.User.Username()
			if username != "" {
				// 保留用户名便于识别连接目标，刻意丢弃密码，避免 prompt 泄密。
				parsed.User = url.User(username)
			} else {
				parsed.User = nil
			}
		}
		return parsed.String()
	}
	return tools.RedactSensitive(dsn)
}

// toolSchemasFromRegistry 把工具注册表中的参数 schema 复制给 policy。
// runtime 先用这些 schema 拦截未知参数和基础类型错误，再真正执行工具。
func toolSchemasFromRegistry(registry *tools.Registry) map[string]tools.ToolSchema {
	schemas := map[string]tools.ToolSchema{}
	for _, spec := range registry.List() {
		schemas[spec.Name] = spec.Schema
	}
	return schemas
}

// resolveDiagnosisConfig 按入口参数优先、配置文件其次、默认值最后的顺序补齐诊断运行配置。
func resolveDiagnosisConfig(opts diagnoseOptions) (diagnosisConfig, error) {
	if strings.TrimSpace(opts.Goal) == "" {
		return diagnosisConfig{}, fmt.Errorf("goal is required")
	}

	cfg := appconfig.Default()
	if opts.ConfigPath != "" {
		// 配置文件只覆盖写明的字段，未配置字段继续使用 Default。
		loaded, err := appconfig.Load(opts.ConfigPath)
		if err != nil {
			return diagnosisConfig{}, err
		}
		cfg = loaded
	}

	allowedLogDirs := cfg.Policy.AllowedLogDirs
	allowedLogDir := ""
	if len(allowedLogDirs) > 0 {
		allowedLogDir = allowedLogDirs[0]
	} else {
		// 没有显式 allowlist 时，只允许读取配置日志文件所在目录，
		// 避免 log_read 因默认空目录而失去路径边界。
		allowedLogDir = filepath.Dir(cfg.Targets.LogFile)
		allowedLogDirs = []string{allowedLogDir}
	}
	resolved := diagnosisConfig{
		Goal:              opts.Goal,
		BackendBaseURL:    cfg.Targets.BackendBaseURL,
		LogFile:           cfg.Targets.LogFile,
		AllowedLogDir:     allowedLogDir,
		AllowedLogDirs:    allowedLogDirs,
		AllowedHosts:      cfg.Policy.AllowedHosts,
		AllowedContainers: cfg.Policy.AllowedContainers,
		PostgresDSN:       cfg.Targets.PostgresDSN,
		RedisAddr:         cfg.Targets.RedisAddr,
		WebSocketURL:      cfg.Targets.WebSocketURL,
		MaxSteps:          cfg.Agent.MaxSteps,
		LLMTimeout:        cfg.Agent.LLMTimeout,
		ToolTimeout:       cfg.Agent.ToolTimeout,
		ToolAllowlist:     cfg.Policy.ToolAllowlist,
		RunDir:            cfg.Paths.RunDir,
		ReportDir:         cfg.Paths.ReportDir,
	}
	if opts.BackendBaseURL != "" {
		resolved.BackendBaseURL = opts.BackendBaseURL
	}
	if opts.LogFile != "" {
		resolved.LogFile = opts.LogFile
	}
	if len(opts.AllowedLogDirs) > 0 {
		resolved.AllowedLogDirs = opts.AllowedLogDirs
		resolved.AllowedLogDir = opts.AllowedLogDirs[0]
	}
	if opts.AllowedLogDir != "" {
		resolved.AllowedLogDir = opts.AllowedLogDir
		resolved.AllowedLogDirs = []string{opts.AllowedLogDir}
	}
	if len(opts.AllowedHosts) > 0 {
		resolved.AllowedHosts = opts.AllowedHosts
	}
	if len(opts.AllowedContainers) > 0 {
		resolved.AllowedContainers = opts.AllowedContainers
	}
	if opts.PostgresDSN != "" {
		resolved.PostgresDSN = opts.PostgresDSN
	}
	if opts.RedisAddr != "" {
		resolved.RedisAddr = opts.RedisAddr
	}
	if opts.WebSocketURL != "" {
		resolved.WebSocketURL = opts.WebSocketURL
	}
	if opts.MaxSteps > 0 {
		resolved.MaxSteps = opts.MaxSteps
	}
	if opts.LLMTimeout > 0 {
		resolved.LLMTimeout = opts.LLMTimeout
	}
	if opts.ToolTimeout > 0 {
		resolved.ToolTimeout = opts.ToolTimeout
	}
	if len(opts.ToolAllowlist) > 0 {
		resolved.ToolAllowlist = opts.ToolAllowlist
	}
	if opts.RunDir != "" {
		resolved.RunDir = opts.RunDir
	}
	if opts.ReportDir != "" {
		resolved.ReportDir = opts.ReportDir
	}

	return resolved, nil
}

func resolveRunDir(configPath string, runDir string) (string, error) {
	if strings.TrimSpace(runDir) != "" {
		return runDir, nil
	}
	cfg := appconfig.Default()
	if strings.TrimSpace(configPath) != "" {
		loaded, err := appconfig.Load(configPath)
		if err != nil {
			return "", err
		}
		cfg = loaded
	}
	return cfg.Paths.RunDir, nil
}

// buildLLMProvider 根据环境配置选择 mock provider 或真实 ActionPlanner。
// runtime 只依赖 llm.Provider，因此后续换模型不需要改 agent 主循环。
func buildLLMProvider(cfg diagnosisConfig, mockScenario string) (llm.Provider, string, error) {
	llmConfig, err := loadLLMConfig()
	if err != nil {
		return nil, "", err
	}

	if mockScenario != "" || llmConfig.Provider == "" || llmConfig.Provider == llm.DefaultLLMProvider {
		if mockScenario == "" {
			mockScenario = "skeleton"
		}
		// mock provider 返回固定 action 序列，用于本地演示和确定性测试；
		// 它仍然会经过 runtime、policy 和真实工具执行链路。
		actions, err := scenarioActions(mockScenario, cfg)
		if err != nil {
			return nil, "", err
		}
		return llm.NewMockProvider(actions), "mock", nil
	}

	client, err := newChatClient(llmConfig)
	if err != nil {
		return nil, "", err
	}
	// 真实 provider 只负责产出结构化 action，工具执行和证据校验仍在本进程内完成。
	return llm.NewActionPlanner(client, llm.ActionPlannerConfig{
		Model:       llmConfig.Model,
		Temperature: 0.2,
	}), llmConfig.Model, nil
}
