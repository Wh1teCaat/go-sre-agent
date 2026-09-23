package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	appconfig "github.com/y2/go-sre-agent/internal/config"
	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/tools/docker"
	"github.com/y2/go-sre-agent/internal/tools/httpcheck"
	"github.com/y2/go-sre-agent/internal/tools/kafka"
	"github.com/y2/go-sre-agent/internal/tools/logread"
	"github.com/y2/go-sre-agent/internal/tools/postgres"
	"github.com/y2/go-sre-agent/internal/tools/redis"
	"github.com/y2/go-sre-agent/internal/tools/smoke"
	"github.com/y2/go-sre-agent/internal/tools/websocket"
)

// diagnosisSetup 保存一次诊断启动后可直接注入 runtime 的依赖。
type diagnosisSetup struct {
	config   diagnoseOptions
	registry *tools.Registry
	provider llm.Provider
	model    string
}

// initializeDiagnosis 解析配置并创建工具注册表与 LLM provider。
// 参数: opts 为 CLI 诊断选项，mockScenario 为可选 mock 场景；返回: 完整启动依赖或初始化错误。
func initializeDiagnosis(opts diagnoseOptions, mockScenario string) (diagnosisSetup, error) {
	cfg, err := resolveDiagnosisConfig(opts)
	setup := diagnosisSetup{config: cfg}
	if err != nil {
		return setup, err
	}
	setup.registry, err = buildToolRegistry(cfg)
	if err != nil {
		return setup, err
	}
	if mockScenario == "tui-demo" {
		if err := registerTUIDemoTools(setup.registry); err != nil {
			return setup, err
		}
	}
	setup.provider, setup.model, err = buildLLMProvider(cfg, mockScenario)
	return setup, err
}

// buildToolRegistry 注册配置边界内允许使用的全部只读诊断工具。
// 参数: cfg 为已解析诊断配置；返回: 工具注册表或注册错误。
func buildToolRegistry(cfg diagnoseOptions) (*tools.Registry, error) {
	registry := tools.NewRegistry()
	dockerScope := docker.NewScope(cfg.DockerComposeProject, cfg.AllowedComposeServices)
	all := []tools.Tool{
		httpcheck.NewWithPolicy(2048, cfg.AllowedHosts, cfg.AllowedPostURLs, cfg.AllowedResponseHeaders),
		logread.New(cfg.AllowedLogDirs, 1000),
		postgres.NewPing(),
		postgres.NewCheck(),
		redis.New(),
		redis.NewCheck(),
		redis.NewScan(cfg.RedisKeyPrefixes),
		kafka.NewCheck(),
		websocket.NewWithAllowedHosts(cfg.AllowedHosts),
		docker.NewPS(cfg.AllowedContainers, dockerScope),
		docker.NewInspect(cfg.AllowedContainers, dockerScope),
		docker.NewLogs(cfg.AllowedContainers, dockerScope),
		docker.NewStats(cfg.AllowedContainers, dockerScope),
		docker.NewProbe(cfg.AllowedContainers, dockerScope),
	}
	if cfg.KafkaContainer != "" && cfg.KafkaTopic != "" && cfg.KafkaConsumerGroup != "" {
		all = append(all, docker.NewKafkaComposeCheck(
			cfg.AllowedContainers,
			cfg.KafkaContainer,
			cfg.KafkaTopic,
			cfg.KafkaConsumerGroup,
			dockerScope,
		))
	}
	// smoke_run 是唯一的非只读探测（合成事务），仅在运营者显式配置命令时注册。
	if len(cfg.SmokeCommand) > 0 {
		all = append(all, smoke.New(cfg.SmokeCommand, cfg.SmokeDir, cfg.SmokeTimeout))
	}
	for _, tool := range all {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// buildLLMProvider 根据环境配置选择 mock provider 或真实 ActionPlanner。
// 参数: cfg 为诊断配置，mockScenario 为可选 mock 场景；返回: provider、模型名和初始化错误。
func buildLLMProvider(cfg diagnoseOptions, mockScenario string) (llm.Provider, string, error) {
	if mockScenario != "" {
		actions, err := scenarioActions(mockScenario, cfg)
		if err != nil {
			return nil, "", err
		}
		return llm.NewMockProvider(actions), "mock", nil
	}

	llmConfig, err := loadLLMConfig()
	if err != nil {
		return nil, "", err
	}
	if llmConfig.Provider == "" || llmConfig.Provider == llm.DefaultLLMProvider {
		actions, err := scenarioActions("skeleton", cfg)
		if err != nil {
			return nil, "", err
		}
		return llm.NewMockProvider(actions), "mock", nil
	}

	client, err := newChatClient(llmConfig)
	if err != nil {
		return nil, "", err
	}
	skill, err := llm.LoadSkill(cfg.SkillPath)
	if err != nil {
		return nil, "", err
	}
	return llm.NewActionPlanner(client, llm.ActionPlannerConfig{
		Model:       llmConfig.Model,
		Temperature: 0.2,
		Skill:       skill,
	}), llmConfig.Model, nil
}

// loadLLMConfig 优先读取当前目录 .env，找不到时回退到进程环境变量。
// 参数: 无；返回: LLM 配置或读取解析错误。
func loadLLMConfig() (llm.Config, error) {
	const envPath = ".env"
	if _, err := os.Stat(envPath); err == nil {
		return llm.LoadConfig(envPath)
	} else if !os.IsNotExist(err) {
		return llm.Config{}, fmt.Errorf("stat env file: %w", err)
	}
	return llm.LoadConfig("")
}

// newChatClient 按 provider 创建对应协议的 LLM 客户端。
// 参数: config 为 LLM provider 配置；返回: ChatClient 或不支持/缺少配置错误。
func newChatClient(config llm.Config) (llm.ChatClient, error) {
	switch config.Provider {
	case "openai_compatible":
		return llm.NewOpenAICompatibleChatClient(config), nil
	case "ollama":
		if strings.TrimSpace(config.Model) == "" {
			return nil, fmt.Errorf("OLLAMA_MODEL is required")
		}
		return llm.NewOpenAICompatibleChatClient(config), nil
	case "anthropic":
		if strings.TrimSpace(config.Model) == "" {
			return nil, fmt.Errorf("ANTHROPIC_MODEL is required")
		}
		return llm.NewAnthropicChatClient(config), nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q", config.Provider)
	}
}

// resolveDiagnosisConfig 按 CLI 覆盖、配置文件、默认值的优先级生成运行配置。
// 参数: opts 为 CLI 诊断选项；返回: 已解析运行配置或配置错误。
func resolveDiagnosisConfig(opts diagnoseOptions) (diagnoseOptions, error) {
	cfg, err := loadAppConfig(opts.ConfigPath)
	if err != nil {
		return diagnoseOptions{}, err
	}

	allowedLogDirs := cfg.Policy.AllowedLogDirs
	if len(allowedLogDirs) == 0 {
		allowedLogDirs = []string{filepath.Dir(cfg.Targets.LogFile)}
	}
	allowedPostURLs := cfg.Targets.AllowedPostURLs
	if len(allowedPostURLs) == 0 {
		// 兼容未配置 targets.allowed_post_urls 的旧配置：沿用登录地址派生规则。
		if loginURL := legacyLoginURL(cfg.Targets.BackendBaseURL); loginURL != "" {
			allowedPostURLs = []string{loginURL}
		}
	}
	resolved := diagnoseOptions{
		Goal:                   opts.Goal,
		Service:                cfg.Targets.Service,
		BackendBaseURL:         cfg.Targets.BackendBaseURL,
		AllowedPostURLs:        allowedPostURLs,
		LogFile:                cfg.Targets.LogFile,
		AllowedLogDirs:         allowedLogDirs,
		AllowedHosts:           cfg.Policy.AllowedHosts,
		AllowedResponseHeaders: cfg.Policy.AllowedResponseHeaders,
		AllowedContainers:      cfg.Policy.AllowedContainers,
		DockerComposeProject:   cfg.Policy.DockerComposeProject,
		AllowedComposeServices: cfg.Policy.AllowedComposeServices,
		RedisKeyPrefixes:       cfg.Policy.RedisKeyPrefixes,
		PostgresDSN:            cfg.Targets.PostgresDSN,
		RedisAddr:              cfg.Targets.RedisAddr,
		KafkaAddr:              cfg.Targets.KafkaAddr,
		KafkaTopic:             cfg.Targets.KafkaTopic,
		KafkaContainer:         cfg.Targets.KafkaContainer,
		KafkaConsumerGroup:     cfg.Targets.KafkaConsumerGroup,
		SmokeCommand:           cfg.Targets.SmokeCommand,
		SmokeDir:               cfg.Targets.SmokeDir,
		SmokeTimeout:           cfg.Targets.SmokeTimeout,
		WebSocketURL:           cfg.Targets.WebSocketURL,
		MaxSteps:               cfg.Agent.MaxSteps,
		LLMTimeout:             cfg.Agent.LLMTimeout,
		ToolTimeout:            cfg.Agent.ToolTimeout,
		TaskTimeout:            cfg.Agent.TaskTimeout,
		MaxToolCalls:           cfg.Agent.MaxToolCalls,
		MaxParallelTools:       cfg.Agent.MaxParallelTools,
		ContextBudgetBytes:     cfg.Agent.ContextBudgetBytes,
		ToolOutputBudgetBytes:  cfg.Agent.ToolOutputBudgetBytes,
		SkillPath:              cfg.Agent.SkillPath,
		ToolAllowlist:          cfg.Policy.ToolAllowlist,
		RunDir:                 cfg.Paths.RunDir,
		SessionID:              opts.SessionID,
		SessionDir:             cfg.Paths.SessionDir,
		MemoryDir:              opts.MemoryDir,
		Environment:            cfg.Targets.Environment,
		NewSession:             opts.NewSession,
		OverwriteSessionMemory: opts.OverwriteSessionMemory,
		ReportDir:              cfg.Paths.ReportDir,
		Progress:               opts.Progress,
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
	if opts.TaskTimeout > 0 {
		resolved.TaskTimeout = opts.TaskTimeout
	}
	if opts.MaxToolCalls > 0 {
		resolved.MaxToolCalls = opts.MaxToolCalls
	}
	if opts.MaxParallelTools > 0 {
		resolved.MaxParallelTools = opts.MaxParallelTools
	}
	if opts.ContextBudgetBytes > 0 {
		resolved.ContextBudgetBytes = opts.ContextBudgetBytes
	}
	if opts.ToolOutputBudgetBytes > 0 {
		resolved.ToolOutputBudgetBytes = opts.ToolOutputBudgetBytes
	}
	if opts.RunDir != "" {
		resolved.RunDir = opts.RunDir
	}
	if opts.SessionDir != "" {
		resolved.SessionDir = opts.SessionDir
	}
	if opts.Service != "" {
		resolved.Service = opts.Service
	}
	if opts.Environment != "" {
		resolved.Environment = opts.Environment
	}
	if err := validateRuntimeLimits(resolved); err != nil {
		return diagnoseOptions{}, err
	}
	return resolved, nil
}

// validateRuntimeLimits 对配置文件和 CLI 覆盖后的最终运行预算统一做边界校验。
func validateRuntimeLimits(config diagnoseOptions) error {
	if config.MaxToolCalls <= 0 {
		return fmt.Errorf("max tool calls must be positive")
	}
	if config.MaxParallelTools <= 0 || config.MaxParallelTools > 8 {
		return fmt.Errorf("max parallel tools must be between 1 and 8")
	}
	if config.ContextBudgetBytes < 1024 {
		return fmt.Errorf("context budget bytes must be at least 1024")
	}
	if config.ToolOutputBudgetBytes < 512 {
		return fmt.Errorf("tool output budget bytes must be at least 512")
	}
	return nil
}

// resolveRunDir 解析 CLI 或配置文件指定的 run 状态目录。
// 参数: configPath 为配置路径，runDir 为 CLI 覆盖值；返回: 最终目录或配置错误。
func resolveRunDir(configPath, runDir string) (string, error) {
	if strings.TrimSpace(runDir) != "" {
		return runDir, nil
	}
	cfg, err := loadAppConfig(configPath)
	if err != nil {
		return "", err
	}
	return cfg.Paths.RunDir, nil
}

// loadAppConfig 加载显式或默认路径下的 YAML 配置。
// 参数: configPath 为 CLI 指定路径；返回: 配置和加载错误。
func loadAppConfig(configPath string) (appconfig.Config, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = "configs/config.yaml"
	}
	return appconfig.Load(configPath)
}

// targetContextForDiagnose 构造不含凭据的 LLM 目标上下文。
// 参数: cfg 为诊断配置；返回: 提供给 LLM 的目标与白名单信息。
func targetContextForDiagnose(cfg diagnoseOptions) map[string]any {
	context := map[string]any{
		"backend_base_url":         cfg.BackendBaseURL,
		"allowed_post_urls":        cfg.AllowedPostURLs,
		"postgres_target":          safePostgresDSNForPrompt(cfg.PostgresDSN),
		"postgres_dsn_configured":  strings.TrimSpace(cfg.PostgresDSN) != "",
		"redis_addr":               cfg.RedisAddr,
		"websocket_url":            cfg.WebSocketURL,
		"log_file":                 cfg.LogFile,
		"allowed_hosts":            cfg.AllowedHosts,
		"allowed_response_headers": cfg.AllowedResponseHeaders,
		"docker_containers":        cfg.AllowedContainers,
		"docker_compose_project":   cfg.DockerComposeProject,
	}
	// 不将不可调用的 Kafka 目标交给模型，避免 Compose 内部 broker 被模型当成宿主机可达地址。
	if toolAllowed(cfg.ToolAllowlist, kafka.CheckName) && strings.TrimSpace(cfg.KafkaAddr) != "" {
		context["kafka_addr"] = cfg.KafkaAddr
		context["kafka_topic"] = cfg.KafkaTopic
	}
	if toolAllowed(cfg.ToolAllowlist, docker.KafkaComposeCheckName) {
		context["kafka_container"] = cfg.KafkaContainer
		context["kafka_topic"] = cfg.KafkaTopic
		context["kafka_consumer_group"] = cfg.KafkaConsumerGroup
	}
	return context
}

// legacyLoginURL 是未配置 targets.allowed_post_urls 时的兼容回退：
// 按 go-chat 的路由约定从后端地址派生登录诊断地址。新配置应显式列出 POST 白名单。
func legacyLoginURL(backendBaseURL string) string {
	if strings.TrimSpace(backendBaseURL) == "" {
		return ""
	}
	return strings.TrimRight(backendBaseURL, "/") + "/v1/user/login"
}

// toolArgOverridesForDiagnose 固定依赖工具参数，防止模型改写配置目标。
// 参数: cfg 为诊断配置；返回: 按工具名组织的强制参数，无覆盖时返回 nil。
func toolArgOverridesForDiagnose(cfg diagnoseOptions) map[string]map[string]any {
	overrides := map[string]map[string]any{}
	if dsn := strings.TrimSpace(cfg.PostgresDSN); dsn != "" {
		overrides[postgres.PingName] = map[string]any{"dsn": dsn}
		overrides[postgres.CheckName] = map[string]any{"dsn": dsn}
	}
	if addr := strings.TrimSpace(cfg.RedisAddr); addr != "" {
		overrides[redis.Name] = map[string]any{"addr": addr}
		overrides[redis.CheckName] = map[string]any{"addr": addr}
		overrides[redis.ScanName] = map[string]any{"addr": addr}
	}
	if addr := strings.TrimSpace(cfg.KafkaAddr); toolAllowed(cfg.ToolAllowlist, kafka.CheckName) && addr != "" {
		kafkaOverrides := map[string]any{"addr": addr}
		if topic := strings.TrimSpace(cfg.KafkaTopic); topic != "" {
			kafkaOverrides["topic"] = topic
		}
		overrides[kafka.CheckName] = kafkaOverrides
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}

// toolAllowed 判断工具是否在本次运行的策略白名单中。
// 参数: allowlist 为运行时白名单，name 为待查询工具名；返回: 工具是否允许调用。
func toolAllowed(allowlist []string, name string) bool {
	for _, allowed := range allowlist {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	return false
}

// safePostgresDSNForPrompt 移除 DSN 密码后再暴露给 LLM。
// 参数: dsn 为原始 PostgreSQL DSN；返回: 脱敏 DSN 或脱敏占位符。
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
			if username := parsed.User.Username(); username != "" {
				parsed.User = url.User(username)
			} else {
				parsed.User = nil
			}
		}
		return parsed.String()
	}
	return tools.RedactSensitive(dsn)
}

// toolSchemasFromRegistry 提取实际注册工具的参数 schema 供 policy 校验。
// 参数: registry 为工具注册表；返回: 工具名到参数 schema 的映射。
func toolSchemasFromRegistry(registry *tools.Registry) map[string]tools.ToolSchema {
	schemas := map[string]tools.ToolSchema{}
	for _, spec := range registry.List() {
		schemas[spec.Name] = spec.Schema
	}
	return schemas
}
