package main

import (
	"os"
	"testing"
)

func TestBuildLLMProviderDoesNotLoadConfigForExplicitMock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/.env", 0o755); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	_, model, err := buildLLMProvider(diagnoseOptions{}, "skeleton")
	if err != nil {
		t.Fatalf("build mock provider: %v", err)
	}
	if model != "mock" {
		t.Fatalf("model = %q, want mock", model)
	}
}
