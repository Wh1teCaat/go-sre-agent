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
  max_tool_calls: 16
  max_parallel_tools: 3
  context_budget_bytes: 16384
  tool_output_budget_bytes: 2048
  skill_path: custom/SKILL.md
policy:
  tool_allowlist:
    - http_check
    - log_read
  allowed_log_dirs:
    - ./logs
  allowed_hosts:
    - localhost
  allowed_response_headers:
    - X-Upstream-Addr
  allowed_containers:
    - chat-backend
  docker_compose_project: chat-test
  allowed_compose_services:
    - backend
paths:
  run_dir: /tmp/sre-agent-runs
  session_dir: /tmp/sre-agent-sessions
  report_dir: /tmp/sre-agent-reports
targets:
  service: go-chat-staging
  backend_base_url: http://localhost:9000
  environment: staging
  postgres_dsn: postgres://app:secret@localhost:5432/chat_proj?sslmode=disable
  redis_addr: localhost:6380
  kafka_container: chat-kafka
  kafka_consumer_group: writers
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
	if cfg.Agent.MaxToolCalls != 16 || cfg.Agent.MaxParallelTools != 3 {
		t.Fatalf("tool call limits = %d/%d, want 16/3", cfg.Agent.MaxToolCalls, cfg.Agent.MaxParallelTools)
	}
	if cfg.Agent.ContextBudgetBytes != 16384 || cfg.Agent.ToolOutputBudgetBytes != 2048 {
		t.Fatalf("context limits = %d/%d, want 16384/2048", cfg.Agent.ContextBudgetBytes, cfg.Agent.ToolOutputBudgetBytes)
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
	if got := cfg.Policy.AllowedResponseHeaders; len(got) != 1 || got[0] != "X-Upstream-Addr" {
		t.Fatalf("allowed response headers = %#v, want X-Upstream-Addr", got)
	}
	if cfg.Policy.DockerComposeProject != "chat-test" || !contains(cfg.Policy.AllowedComposeServices, "backend") {
		t.Fatalf("Compose scope = %q / %#v", cfg.Policy.DockerComposeProject, cfg.Policy.AllowedComposeServices)
	}
	if cfg.Targets.KafkaContainer != "chat-kafka" || cfg.Targets.KafkaConsumerGroup != "writers" {
		t.Fatalf("Kafka Compose target = %q / %q", cfg.Targets.KafkaContainer, cfg.Targets.KafkaConsumerGroup)
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
	if cfg.Targets.Service != "go-chat-staging" {
		t.Fatalf("service = %q, want go-chat-staging", cfg.Targets.Service)
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
	if cfg.Agent.MaxToolCalls != 24 || cfg.Agent.MaxParallelTools != 2 || cfg.Agent.ContextBudgetBytes != 48*1024 || cfg.Agent.ToolOutputBudgetBytes != 8*1024 {
		t.Fatalf("default runtime limits = %#v", cfg.Agent)
	}
	if cfg.Paths.SessionDir != ".sessions" || cfg.Targets.Environment != "local" || cfg.Targets.Service != "go-chat" {
		t.Fatalf("default session settings = %#v / %q / %q", cfg.Paths, cfg.Targets.Environment, cfg.Targets.Service)
	}
	if cfg.Targets.KafkaAddr != "" || cfg.Targets.KafkaTopic != "chat-messages" {
		t.Fatalf("default Kafka target = %q / %q, want no host address and chat-messages topic", cfg.Targets.KafkaAddr, cfg.Targets.KafkaTopic)
	}
	if cfg.Targets.KafkaContainer != "chat-kafka" || cfg.Targets.KafkaConsumerGroup != "go-chat-message-writers" {
		t.Fatalf("default Kafka Compose target = %q / %q", cfg.Targets.KafkaContainer, cfg.Targets.KafkaConsumerGroup)
	}
	for _, want := range []string{"chat-edge", "go-chat-backend-1", "go-chat-backend-2", "chat-kafka"} {
		if !contains(cfg.Policy.AllowedContainers, want) {
			t.Fatalf("default allowed containers = %#v, want %q", cfg.Policy.AllowedContainers, want)
		}
	}

	for _, want := range []string{"localhost", "127.0.0.1", "::1"} {
		if !contains(cfg.Policy.AllowedHosts, want) {
			t.Fatalf("default allowed hosts = %#v, want %q", cfg.Policy.AllowedHosts, want)
		}
	}
}

// TestGoChatComposeConfigDisablesUnreachableKafka 验证随仓库提供的 Go Chat Compose
// 配置不会把仅容器网络可达的 Kafka 或会写入业务数据的冒烟工具交给模型。
func TestGoChatComposeConfigDisablesUnreachableKafka(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "go-chat-compose.example.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load Go Chat Compose config: %v", err)
	}
	for _, forbidden := range []string{"kafka_check", "smoke_run"} {
		if contains(cfg.Policy.ToolAllowlist, forbidden) {
			t.Fatalf("Go Chat Compose config must not enable %q: %#v", forbidden, cfg.Policy.ToolAllowlist)
		}
	}
	if !contains(cfg.Policy.ToolAllowlist, "kafka_compose_check") {
		t.Fatalf("Go Chat Compose config must enable kafka_compose_check: %#v", cfg.Policy.ToolAllowlist)
	}
	if cfg.Policy.DockerComposeProject != "go-chat" || !contains(cfg.Policy.AllowedComposeServices, "backend") {
		t.Fatalf("Go Chat Compose scope = %q / %#v", cfg.Policy.DockerComposeProject, cfg.Policy.AllowedComposeServices)
	}
	if cfg.Targets.KafkaTopic != "chat-messages" {
		t.Fatalf("Kafka topic = %q, want chat-messages", cfg.Targets.KafkaTopic)
	}
	for _, want := range []string{"chat-edge", "go-chat-backend-1", "go-chat-backend-2", "chat-kafka"} {
		if !contains(cfg.Policy.AllowedContainers, want) {
			t.Fatalf("Go Chat Compose container allowlist = %#v, want %q", cfg.Policy.AllowedContainers, want)
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

func TestLoadRejectsInvalidRuntimeBudgets(t *testing.T) {
	for _, config := range []string{
		"agent:\n  max_parallel_tools: 9\n",
		"agent:\n  context_budget_bytes: 1023\n",
		"agent:\n  tool_output_budget_bytes: 511\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("expected runtime budget validation for %q", config)
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
