package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadReadsYAMLConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
agent:
  max_steps: 12
  llm_timeout: 25s
  tool_timeout: 7s
  task_timeout: 3m
  skill_path: custom/SKILL.md
policy:
  tool_allowlist:
    - http_check
    - log_read
  allowed_log_dirs:
    - ./logs
  allowed_hosts:
    - localhost
  allowed_containers:
    - chat-backend
paths:
  run_dir: /tmp/sre-agent-runs
  session_dir: /tmp/sre-agent-sessions
  report_dir: /tmp/sre-agent-reports
targets:
  backend_base_url: http://localhost:9000
  environment: staging
  postgres_dsn: postgres://app:secret@localhost:5432/chat_proj?sslmode=disable
  redis_addr: localhost:6380
  websocket_url: ws://localhost:9000/ws
  log_file: ./logs/app.log
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if cfg.Agent.MaxSteps != 12 {
		t.Fatalf("max steps = %d, want 12", cfg.Agent.MaxSteps)
	}
	if cfg.Agent.ToolTimeout != 7*time.Second {
		t.Fatalf("tool timeout = %s, want 7s", cfg.Agent.ToolTimeout)
	}
	if cfg.Agent.LLMTimeout != 25*time.Second {
		t.Fatalf("llm timeout = %s, want 25s", cfg.Agent.LLMTimeout)
	}
	if cfg.Agent.TaskTimeout != 3*time.Minute {
		t.Fatalf("task timeout = %s, want 3m", cfg.Agent.TaskTimeout)
	}
	if cfg.Agent.SkillPath != "custom/SKILL.md" {
		t.Fatalf("skill path = %q, want custom/SKILL.md", cfg.Agent.SkillPath)
	}
	if got := cfg.Policy.ToolAllowlist; len(got) != 2 || got[0] != "http_check" || got[1] != "log_read" {
		t.Fatalf("tool allowlist = %#v, want http_check/log_read", got)
	}
	if cfg.Targets.BackendBaseURL != "http://localhost:9000" {
		t.Fatalf("backend base URL = %q", cfg.Targets.BackendBaseURL)
	}
	if got := cfg.Policy.AllowedContainers; len(got) != 1 || got[0] != "chat-backend" {
		t.Fatalf("allowed containers = %#v, want chat-backend", got)
	}
	if cfg.Targets.WebSocketURL != "ws://localhost:9000/ws" {
		t.Fatalf("websocket URL = %q", cfg.Targets.WebSocketURL)
	}
	if cfg.Paths.RunDir != "/tmp/sre-agent-runs" {
		t.Fatalf("run dir = %q, want configured value", cfg.Paths.RunDir)
	}
	if cfg.Paths.SessionDir != "/tmp/sre-agent-sessions" {
		t.Fatalf("session dir = %q, want configured value", cfg.Paths.SessionDir)
	}
	if cfg.Paths.ReportDir != "/tmp/sre-agent-reports" {
		t.Fatalf("report dir = %q, want configured value", cfg.Paths.ReportDir)
	}
	if cfg.Targets.Environment != "staging" {
		t.Fatalf("environment = %q, want staging", cfg.Targets.Environment)
	}
}

func TestDefaultAllowsLocalDiagnosticHosts(t *testing.T) {
	cfg := Default()
	if cfg.Agent.SkillPath != "skills/sre-diagnosis/SKILL.md" {
		t.Fatalf("default skill path = %q", cfg.Agent.SkillPath)
	}
	if cfg.Agent.TaskTimeout != 5*time.Minute {
		t.Fatalf("default task timeout = %s, want 5m", cfg.Agent.TaskTimeout)
	}
	if cfg.Paths.SessionDir != ".sessions" || cfg.Targets.Environment != "local" {
		t.Fatalf("default session settings = %#v / %q", cfg.Paths, cfg.Targets.Environment)
	}

	for _, want := range []string{"localhost", "127.0.0.1", "::1"} {
		if !contains(cfg.Policy.AllowedHosts, want) {
			t.Fatalf("default allowed hosts = %#v, want %q", cfg.Policy.AllowedHosts, want)
		}
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
agent:
  tool_timeout: definitely-not-a-duration
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid duration to fail")
	}
}

func TestLoadRejectsNonPositiveDuration(t *testing.T) {
	for _, config := range []string{
		"agent:\n  llm_timeout: 0s\n",
		"agent:\n  tool_timeout: -1s\n",
		"agent:\n  task_timeout: 0s\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("expected non-positive duration in %q to fail", config)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
