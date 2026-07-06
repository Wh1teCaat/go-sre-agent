package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Agent   AgentConfig
	Policy  PolicyConfig
	Targets TargetConfig
}

type AgentConfig struct {
	MaxSteps    int
	ToolTimeout time.Duration
}

type PolicyConfig struct {
	ToolAllowlist  []string
	AllowedLogDirs []string
	AllowedHosts   []string
}

type TargetConfig struct {
	BackendBaseURL string
	PostgresDSN    string
	RedisAddr      string
	WebSocketURL   string
	LogFile        string
}

type rawConfig struct {
	Agent struct {
		MaxSteps    int    `yaml:"max_steps"`
		ToolTimeout string `yaml:"tool_timeout"`
	} `yaml:"agent"`
	Policy struct {
		ToolAllowlist  []string `yaml:"tool_allowlist"`
		AllowedLogDirs []string `yaml:"allowed_log_dirs"`
		AllowedHosts   []string `yaml:"allowed_hosts"`
	} `yaml:"policy"`
	Targets struct {
		BackendBaseURL string `yaml:"backend_base_url"`
		PostgresDSN    string `yaml:"postgres_dsn"`
		RedisAddr      string `yaml:"redis_addr"`
		WebSocketURL   string `yaml:"websocket_url"`
		LogFile        string `yaml:"log_file"`
	} `yaml:"targets"`
}

// Default 给 CLI 提供一个能在本机跑通的最小配置。
// 生产或项目特定目标应通过 config.yaml 或 CLI 参数覆盖。
func Default() Config {
	return Config{
		Agent: AgentConfig{
			MaxSteps:    8,
			ToolTimeout: 5 * time.Second,
		},
		Policy: PolicyConfig{
			ToolAllowlist: []string{
				"http_check",
				"log_read",
				"redis_ping",
				"postgres_ping",
				"websocket_check",
			},
			AllowedHosts: []string{
				"localhost",
				"127.0.0.1",
				"::1",
			},
		},
		Targets: TargetConfig{
			BackendBaseURL: "http://localhost:8080",
			PostgresDSN:    "postgres://postgres:postgres@localhost:5432/chat_proj?sslmode=disable",
			RedisAddr:      "localhost:6379",
			WebSocketURL:   "ws://localhost:8080/ws",
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
	if raw.Agent.ToolTimeout != "" {
		duration, err := time.ParseDuration(raw.Agent.ToolTimeout)
		if err != nil {
			return Config{}, fmt.Errorf("parse agent.tool_timeout: %w", err)
		}
		cfg.Agent.ToolTimeout = duration
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
	if raw.Targets.BackendBaseURL != "" {
		cfg.Targets.BackendBaseURL = raw.Targets.BackendBaseURL
	}
	if raw.Targets.PostgresDSN != "" {
		cfg.Targets.PostgresDSN = raw.Targets.PostgresDSN
	}
	if raw.Targets.RedisAddr != "" {
		cfg.Targets.RedisAddr = raw.Targets.RedisAddr
	}
	if raw.Targets.WebSocketURL != "" {
		cfg.Targets.WebSocketURL = raw.Targets.WebSocketURL
	}
	if raw.Targets.LogFile != "" {
		cfg.Targets.LogFile = raw.Targets.LogFile
	}

	return cfg, nil
}
