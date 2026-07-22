package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/y2/go-sre-agent/internal/schema"
)

type fakeTool struct {
	name string
}

func (f fakeTool) Spec() ToolSpec {
	return ToolSpec{Name: f.name, Description: "fake tool", Schema: ToolSchema{
		Properties: map[string]ArgSpec{"target": {Type: "string", Required: true}},
	}}
}

func (f fakeTool) Run(context.Context, json.RawMessage) (schema.Observation, error) {
	return schema.Observation{Tool: f.name, Summary: "ok"}, nil
}

func TestRegistryRegistersAndFindsTool(t *testing.T) {
	registry := NewRegistry()

	if err := registry.Register(fakeTool{name: "http_check"}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	tool, ok := registry.Get("http_check")
	if !ok {
		t.Fatal("expected registered tool to be found")
	}
	if tool.Spec().Name != "http_check" {
		t.Fatalf("tool name = %q, want %q", tool.Spec().Name, "http_check")
	}
}

func TestRegistryRejectsDuplicateToolName(t *testing.T) {
	registry := NewRegistry()

	if err := registry.Register(fakeTool{name: "log_read"}); err != nil {
		t.Fatalf("register first tool: %v", err)
	}
	if err := registry.Register(fakeTool{name: "log_read"}); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
}

func TestRegistryListsToolSpecs(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(fakeTool{name: "redis_ping"}); err != nil {
		t.Fatalf("register tool: %v", err)
	}

	specs := registry.List()
	if len(specs) != 1 {
		t.Fatalf("len(specs) = %d, want 1", len(specs))
	}
	if specs[0].Name != "redis_ping" {
		t.Fatalf("spec name = %q, want %q", specs[0].Name, "redis_ping")
	}
}
