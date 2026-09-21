package main

import (
	"path/filepath"
	"testing"

	runstore "github.com/y2/go-sre-agent/internal/run"
)

// TestResolveMemoryScope 使用配置补足未显式指定的检索隔离范围。
func TestResolveMemoryScopeUsesConfiguredDefaults(t *testing.T) {
	configPath := writeTestConfig(t, "targets:\n  service: configured-chat\n  environment: staging\n")
	service, environment, err := resolveMemoryScope("", "", configPath)
	if err != nil {
		t.Fatalf("resolve memory scope: %v", err)
	}
	if service != "configured-chat" || environment != "staging" {
		t.Fatalf("memory scope = %q/%q", service, environment)
	}

	service, environment, err = resolveMemoryScope("manual-chat", "", configPath)
	if err != nil {
		t.Fatalf("resolve partial memory scope: %v", err)
	}
	if service != "manual-chat" || environment != "staging" {
		t.Fatalf("partial memory scope = %q/%q", service, environment)
	}
}

// TestFillLegacyMemoryScope 保留已有 run 范围，并只为人工收录的旧 run 补足配置标签。
func TestFillLegacyMemoryScopePreservesPersistedScope(t *testing.T) {
	configPath := writeTestConfig(t, "targets:\n  service: configured-chat\n  environment: staging\n")
	legacy, err := fillLegacyMemoryScope(runstore.State{RunID: "run_legacy"}, configPath)
	if err != nil {
		t.Fatalf("fill legacy memory scope: %v", err)
	}
	if legacy.Service != "configured-chat" || legacy.Environment != "staging" {
		t.Fatalf("legacy scope = %q/%q", legacy.Service, legacy.Environment)
	}

	persisted := runstore.State{RunID: "run_persisted", Service: "original-chat", Environment: "prod"}
	persisted, err = fillLegacyMemoryScope(persisted, filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("preserve persisted memory scope: %v", err)
	}
	if persisted.Service != "original-chat" || persisted.Environment != "prod" {
		t.Fatalf("persisted scope = %q/%q", persisted.Service, persisted.Environment)
	}
}
