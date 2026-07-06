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
  tool_timeout: 7s
policy:
  tool_allowlist:
    - http_check
    - log_read
  allowed_log_dirs:
    - ./logs
  allowed_hosts:
    - localhost
targets:
  backend_base_url: http://localhost:9000
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
	if got := cfg.Policy.ToolAllowlist; len(got) != 2 || got[0] != "http_check" || got[1] != "log_read" {
		t.Fatalf("tool allowlist = %#v, want http_check/log_read", got)
	}
	if cfg.Targets.BackendBaseURL != "http://localhost:9000" {
		t.Fatalf("backend base URL = %q", cfg.Targets.BackendBaseURL)
	}
	if cfg.Targets.WebSocketURL != "ws://localhost:9000/ws" {
		t.Fatalf("websocket URL = %q", cfg.Targets.WebSocketURL)
	}
}

func TestDefaultAllowsLocalDiagnosticHosts(t *testing.T) {
	cfg := Default()

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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
