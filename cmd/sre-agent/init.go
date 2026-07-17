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
	"github.com/y2/go-sre-agent/internal/tools/logread"
	"github.com/y2/go-sre-agent/internal/tools/postgres"
	"github.com/y2/go-sre-agent/internal/tools/redis"
	"github.com/y2/go-sre-agent/internal/tools/websocket"
)

type diagnosisSetup struct {
	config   diagnoseOptions
	registry *tools.Registry
	provider llm.Provider
	model    string
}

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
	setup.provider, setup.model, err = buildLLMProvider(cfg, mockScenario)
	return setup, err
}

func buildToolRegistry(cfg diagnoseOptions) (*tools.Registry, error) {
	registry := tools.NewRegistry()
	all := []tools.Tool{
		httpcheck.NewWithPolicy(nil, 2048, cfg.AllowedHosts, []string{loginURLForDiagnose(cfg)}),
		logread.New(cfg.AllowedLogDirs, 1000),
		postgres.NewPing(nil),
		postgres.NewCheck(nil),
		redis.New(nil),
		websocket.NewWithAllowedHosts(nil, cfg.AllowedHosts),
		docker.NewPS(cfg.AllowedContainers),
		docker.NewInspect(cfg.AllowedContainers),
		docker.NewLogs(cfg.AllowedContainers),
	}
	for _, tool := range all {
		if err := registry.Register(tool); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// buildLLMProvider 根据环境配置选择 mock provider 或真实 ActionPlanner。
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
		Skill:       skill.Content,
	}), llmConfig.Model, nil
}

// loadLLMConfig 优先读取当前目录 .env，找不到时回退到进程环境变量。
func loadLLMConfig() (llm.Config, error) {
	const envPath = ".env"
	if _, err := os.Stat(envPath); err == nil {
		return llm.LoadConfig(envPath)
	} else if !os.IsNotExist(err) {
		return llm.Config{}, fmt.Errorf("stat env file: %w", err)
	}
	return llm.LoadConfig("")
}

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

func resolveDiagnosisConfig(opts diagnoseOptions) (diagnoseOptions, error) {
	if strings.TrimSpace(opts.Goal) == "" {
		return diagnoseOptions{}, fmt.Errorf("goal is required")
	}

	configPath := resolveConfigPath(opts.ConfigPath)
	cfg := appconfig.Default()
	if configPath != "" {
		loaded, err := appconfig.Load(configPath)
		if err != nil {
			return diagnoseOptions{}, err
		}
		cfg = loaded
	}

	allowedLogDirs := cfg.Policy.AllowedLogDirs
	if len(allowedLogDirs) == 0 {
		allowedLogDirs = []string{filepath.Dir(cfg.Targets.LogFile)}
	}
	resolved := diagnoseOptions{
		Goal:              opts.Goal,
		BackendBaseURL:    cfg.Targets.BackendBaseURL,
		LogFile:           cfg.Targets.LogFile,
		AllowedLogDirs:    allowedLogDirs,
		AllowedHosts:      cfg.Policy.AllowedHosts,
		AllowedContainers: cfg.Policy.AllowedContainers,
		PostgresDSN:       cfg.Targets.PostgresDSN,
		RedisAddr:         cfg.Targets.RedisAddr,
		WebSocketURL:      cfg.Targets.WebSocketURL,
		MaxSteps:          cfg.Agent.MaxSteps,
		LLMTimeout:        cfg.Agent.LLMTimeout,
		ToolTimeout:       cfg.Agent.ToolTimeout,
		SkillPath:         resolveSkillPath(configPath, cfg.Agent.SkillPath),
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

func resolveRunDir(configPath, runDir string) (string, error) {
	if strings.TrimSpace(runDir) != "" {
		return runDir, nil
	}
	cfg := appconfig.Default()
	if configPath = resolveConfigPath(configPath); configPath != "" {
		loaded, err := appconfig.Load(configPath)
		if err != nil {
			return "", err
		}
		cfg = loaded
	}
	return cfg.Paths.RunDir, nil
}

func resolveConfigPath(configPath string) string {
	if strings.TrimSpace(configPath) != "" {
		return configPath
	}
	if _, err := os.Stat("configs/config.yaml"); !os.IsNotExist(err) {
		return "configs/config.yaml"
	}
	return ""
}

func resolveSkillPath(configPath, skillPath string) string {
	skillPath = strings.TrimSpace(skillPath)
	if skillPath == "" || filepath.IsAbs(skillPath) {
		return skillPath
	}
	if _, err := os.Stat(skillPath); err == nil {
		return skillPath
	}
	if configPath != "" {
		candidate := filepath.Join(filepath.Dir(configPath), skillPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		for dir := cwd; ; dir = filepath.Dir(dir) {
			candidate := filepath.Join(dir, skillPath)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	return skillPath
}

func targetContextForDiagnose(cfg diagnoseOptions) map[string]any {
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

func loginURLForDiagnose(cfg diagnoseOptions) string {
	if strings.TrimSpace(cfg.BackendBaseURL) == "" {
		return ""
	}
	return strings.TrimRight(cfg.BackendBaseURL, "/") + "/v1/user/login"
}

func toolArgOverridesForDiagnose(cfg diagnoseOptions) map[string]map[string]any {
	overrides := map[string]map[string]any{}
	if dsn := strings.TrimSpace(cfg.PostgresDSN); dsn != "" {
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

func toolSchemasFromRegistry(registry *tools.Registry) map[string]tools.ToolSchema {
	schemas := map[string]tools.ToolSchema{}
	for _, spec := range registry.List() {
		schemas[spec.Name] = spec.Schema
	}
	return schemas
}
