package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Agent   AgentConfig
	Paths   PathsConfig
	Policy  PolicyConfig
	Targets TargetConfig
}

type AgentConfig struct {
	MaxSteps              int
	LLMTimeout            time.Duration
	ToolTimeout           time.Duration
	TaskTimeout           time.Duration
	MaxToolCalls          int
	MaxParallelTools      int
	ContextBudgetBytes    int
	ToolOutputBudgetBytes int
	SkillPath             string
}

type PolicyConfig struct {
	ToolAllowlist     []string
	AllowedLogDirs    []string
	AllowedHosts      []string
	AllowedContainers []string
	// RedisKeyPrefixes 限定 redis_scan 可查询的键前缀，避免把业务数据暴露进模型上下文。
	RedisKeyPrefixes []string
}

type PathsConfig struct {
	RunDir     string
	SessionDir string
	ReportDir  string
}

type TargetConfig struct {
	// Service 是跨会话知识检索的服务范围标签，不得填写地址、凭据等敏感信息。
	Service        string
	BackendBaseURL string
	// Environment 是 local、staging、prod 等非敏感标签；它限定会话记忆范围，
	// 防止意外跨环境复用。
	Environment string
	// AllowedPostURLs 是 http_check 允许 POST 复现的诊断地址；GET/HEAD 不受限制。
	// 未配置时回退为 backend_base_url 派生的登录地址（见 cmd 层 legacyLoginURL）。
	AllowedPostURLs []string
	PostgresDSN     string
	RedisAddr       string
	KafkaAddr       string
	KafkaTopic      string
	WebSocketURL    string
	LogFile         string
	// SmokeCommand 非空时启用 smoke_run 合成事务工具。
	// 这是唯一的非只读探测（测试账号真实写入），必须由运营者显式配置。
	SmokeCommand []string
	SmokeDir     string
	SmokeTimeout time.Duration
}

type rawConfig struct {
	Agent struct {
		MaxSteps              int    `yaml:"max_steps"`
		LLMTimeout            string `yaml:"llm_timeout"`
		ToolTimeout           string `yaml:"tool_timeout"`
		TaskTimeout           string `yaml:"task_timeout"`
		MaxToolCalls          int    `yaml:"max_tool_calls"`
		MaxParallelTools      int    `yaml:"max_parallel_tools"`
		ContextBudgetBytes    int    `yaml:"context_budget_bytes"`
		ToolOutputBudgetBytes int    `yaml:"tool_output_budget_bytes"`
		SkillPath             string `yaml:"skill_path"`
	} `yaml:"agent"`
	Policy struct {
		ToolAllowlist     []string `yaml:"tool_allowlist"`
		AllowedLogDirs    []string `yaml:"allowed_log_dirs"`
		AllowedHosts      []string `yaml:"allowed_hosts"`
		AllowedContainers []string `yaml:"allowed_containers"`
		RedisKeyPrefixes  []string `yaml:"redis_key_prefixes"`
	} `yaml:"policy"`
	Paths struct {
		RunDir     string `yaml:"run_dir"`
		SessionDir string `yaml:"session_dir"`
		ReportDir  string `yaml:"report_dir"`
	} `yaml:"paths"`
	Targets struct {
		Service         string   `yaml:"service"`
		BackendBaseURL  string   `yaml:"backend_base_url"`
		Environment     string   `yaml:"environment"`
		AllowedPostURLs []string `yaml:"allowed_post_urls"`
		PostgresDSN     string   `yaml:"postgres_dsn"`
		RedisAddr       string   `yaml:"redis_addr"`
		KafkaAddr       string   `yaml:"kafka_addr"`
		KafkaTopic      string   `yaml:"kafka_topic"`
		WebSocketURL    string   `yaml:"websocket_url"`
		LogFile         string   `yaml:"log_file"`
		SmokeCommand    []string `yaml:"smoke_command"`
		SmokeDir        string   `yaml:"smoke_dir"`
		SmokeTimeout    string   `yaml:"smoke_timeout"`
	} `yaml:"targets"`
}

// Default 给 CLI 提供一个能在本机跑通的最小配置。
// 生产或项目特定目标应通过 config.yaml 或 CLI 参数覆盖。
func Default() Config {
	return Config{
		Agent: AgentConfig{
			MaxSteps:              12,
			LLMTimeout:            30 * time.Second,
			ToolTimeout:           5 * time.Second,
			TaskTimeout:           5 * time.Minute,
			MaxToolCalls:          24,
			MaxParallelTools:      2,
			ContextBudgetBytes:    48 * 1024,
			ToolOutputBudgetBytes: 8 * 1024,
			SkillPath:             "skills/sre-diagnosis/SKILL.md",
		},
		Policy: PolicyConfig{
			ToolAllowlist: []string{
				"http_check",
				"log_read",
				"redis_ping",
				"redis_check",
				"redis_scan",
				"kafka_check",
				"postgres_ping",
				"postgres_check",
				"websocket_check",
				"docker_ps",
				"docker_inspect",
				"docker_logs",
				"docker_stats",
				"docker_probe",
				"smoke_run",
			},
			AllowedHosts: []string{
				"localhost",
				"127.0.0.1",
				"::1",
			},
			AllowedContainers: []string{
				"chat-backend",
				"chat-frontend",
				"chat-postgres",
				"chat-redis-compose",
			},
			RedisKeyPrefixes: []string{
				"presence:",
				"auth:refresh:",
				"user:profile:",
			},
		},
		Paths: PathsConfig{
			RunDir:     ".runs",
			SessionDir: ".sessions",
		},
		Targets: TargetConfig{
			Service:        "go-chat",
			BackendBaseURL: "http://localhost:8080",
			Environment:    "local",
			PostgresDSN:    "postgres://postgres:postgres@localhost:5432/chat_proj?sslmode=disable",
			RedisAddr:      "localhost:6379",
			KafkaAddr:      "localhost:29092",
			KafkaTopic:     "chat.events",
			WebSocketURL:   "ws://localhost:8080/v1/ws",
			LogFile:        "testdata/logs/chat_proj_error.log",
		},
	}
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config yaml: %w", err)
	}

	cfg := Default()
	// 配置文件采用“局部覆盖”语义：只写需要调整的字段即可，
	// 未写字段继续沿用 Default，避免示例配置必须复制完整结构。
	if raw.Agent.MaxSteps > 0 {
		cfg.Agent.MaxSteps = raw.Agent.MaxSteps
	}
	if raw.Agent.MaxToolCalls > 0 {
		cfg.Agent.MaxToolCalls = raw.Agent.MaxToolCalls
	}
	if raw.Agent.MaxParallelTools > 0 {
		if raw.Agent.MaxParallelTools > 8 {
			return Config{}, fmt.Errorf("agent.max_parallel_tools must not exceed 8")
		}
		cfg.Agent.MaxParallelTools = raw.Agent.MaxParallelTools
	}
	if raw.Agent.ContextBudgetBytes > 0 {
		if raw.Agent.ContextBudgetBytes < 1024 {
			return Config{}, fmt.Errorf("agent.context_budget_bytes must be at least 1024")
		}
		cfg.Agent.ContextBudgetBytes = raw.Agent.ContextBudgetBytes
	}
	if raw.Agent.ToolOutputBudgetBytes > 0 {
		if raw.Agent.ToolOutputBudgetBytes < 512 {
			return Config{}, fmt.Errorf("agent.tool_output_budget_bytes must be at least 512")
		}
		cfg.Agent.ToolOutputBudgetBytes = raw.Agent.ToolOutputBudgetBytes
	}
	if raw.Agent.LLMTimeout != "" {
		duration, err := time.ParseDuration(raw.Agent.LLMTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("parse agent.llm_timeout: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("agent.llm_timeout must be positive")
		}
		cfg.Agent.LLMTimeout = duration
	}
	if raw.Agent.ToolTimeout != "" {
		duration, err := time.ParseDuration(raw.Agent.ToolTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("parse agent.tool_timeout: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("agent.tool_timeout must be positive")
		}
		cfg.Agent.ToolTimeout = duration
	}
	if raw.Agent.TaskTimeout != "" {
		duration, err := time.ParseDuration(raw.Agent.TaskTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("parse agent.task_timeout: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("agent.task_timeout must be positive")
		}
		cfg.Agent.TaskTimeout = duration
	}
	if raw.Agent.SkillPath != "" {
		cfg.Agent.SkillPath = raw.Agent.SkillPath
	}
	if len(raw.Policy.ToolAllowlist) > 0 {
		cfg.Policy.ToolAllowlist = raw.Policy.ToolAllowlist
	}
	if len(raw.Policy.AllowedLogDirs) > 0 {
		cfg.Policy.AllowedLogDirs = raw.Policy.AllowedLogDirs
	}
	if len(raw.Policy.AllowedHosts) > 0 {
		cfg.Policy.AllowedHosts = raw.Policy.AllowedHosts
	}
	if len(raw.Policy.AllowedContainers) > 0 {
		cfg.Policy.AllowedContainers = raw.Policy.AllowedContainers
	}
	if len(raw.Policy.RedisKeyPrefixes) > 0 {
		cfg.Policy.RedisKeyPrefixes = raw.Policy.RedisKeyPrefixes
	}
	if raw.Paths.RunDir != "" {
		cfg.Paths.RunDir = raw.Paths.RunDir
	}
	if raw.Paths.SessionDir != "" {
		cfg.Paths.SessionDir = raw.Paths.SessionDir
	}
	if raw.Paths.ReportDir != "" {
		cfg.Paths.ReportDir = raw.Paths.ReportDir
	}
	if raw.Targets.BackendBaseURL != "" {
		cfg.Targets.BackendBaseURL = raw.Targets.BackendBaseURL
	}
	if raw.Targets.Service != "" {
		cfg.Targets.Service = raw.Targets.Service
	}
	if raw.Targets.Environment != "" {
		cfg.Targets.Environment = raw.Targets.Environment
	}
	if len(raw.Targets.AllowedPostURLs) > 0 {
		cfg.Targets.AllowedPostURLs = raw.Targets.AllowedPostURLs
	}
	if raw.Targets.PostgresDSN != "" {
		cfg.Targets.PostgresDSN = raw.Targets.PostgresDSN
	}
	if raw.Targets.RedisAddr != "" {
		cfg.Targets.RedisAddr = raw.Targets.RedisAddr
	}
	if raw.Targets.KafkaAddr != "" {
		cfg.Targets.KafkaAddr = raw.Targets.KafkaAddr
	}
	if raw.Targets.KafkaTopic != "" {
		cfg.Targets.KafkaTopic = raw.Targets.KafkaTopic
	}
	if raw.Targets.WebSocketURL != "" {
		cfg.Targets.WebSocketURL = raw.Targets.WebSocketURL
	}
	if raw.Targets.LogFile != "" {
		cfg.Targets.LogFile = raw.Targets.LogFile
	}
	if len(raw.Targets.SmokeCommand) > 0 {
		cfg.Targets.SmokeCommand = raw.Targets.SmokeCommand
	}
	if raw.Targets.SmokeDir != "" {
		cfg.Targets.SmokeDir = raw.Targets.SmokeDir
	}
	if raw.Targets.SmokeTimeout != "" {
		duration, err := time.ParseDuration(raw.Targets.SmokeTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("parse targets.smoke_timeout: %w", err)
		}
		if duration <= 0 {
			return Config{}, fmt.Errorf("targets.smoke_timeout must be positive")
		}
		cfg.Targets.SmokeTimeout = duration
	}

	return cfg, nil
}
