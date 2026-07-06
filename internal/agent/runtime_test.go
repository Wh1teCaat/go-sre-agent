package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type runtimeTool struct{}

func (runtimeTool) Name() string { return "http_check" }

func (runtimeTool) Description() string { return "check HTTP endpoint" }

func (runtimeTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{
		Properties: map[string]tools.ArgSpec{
			"url": {Type: "string", Required: true},
		},
	}
}

func (runtimeTool) Run(context.Context, json.RawMessage) (schema.Observation, error) {
	return schema.Observation{
		Tool:    "http_check",
		Summary: "backend returned 200",
		Data: map[string]any{
			"status": 200,
		},
	}, nil
}

func TestRuntimeRunsToolThenFinalAction(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(runtimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := trace.NewMemoryStore()
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type:           "tool_call",
			ThoughtSummary: "check backend health first",
			Tool:           "http_check",
			Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
		},
		{
			Type:           "final",
			ThoughtSummary: "health evidence is enough",
			Final: &schema.Diagnosis{
				Summary: "backend is alive",
				Evidence: []schema.Evidence{
					{Step: 1, Tool: "http_check", Summary: "backend returned 200"},
				},
			},
		},
	})
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
	}), store)

	diagnosis, err := runtime.Run(context.Background(), "check service")
	if err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if diagnosis.Summary != "backend is alive" {
		t.Fatalf("summary = %q, want backend is alive", diagnosis.Summary)
	}
	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("trace entries = %d, want 1", len(entries))
	}
	if entries[0].Error != "" {
		t.Fatalf("trace error = %q, want empty", entries[0].Error)
	}
	if entries[0].Duration <= 0 {
		t.Fatalf("duration = %s, want positive duration", entries[0].Duration)
	}
}

func TestRuntimeRejectsFinalWithoutEvidenceAfterToolExecution(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(runtimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := trace.NewMemoryStore()
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type:           schema.ActionTypeToolCall,
			ThoughtSummary: "check backend health first",
			Tool:           "http_check",
			Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
		},
		{
			Type:           schema.ActionTypeFinal,
			ThoughtSummary: "summary without evidence",
			Final: &schema.Diagnosis{
				Summary: "backend is alive",
			},
		},
	})
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      2,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
	}), store)

	_, err := runtime.Run(context.Background(), "check service")
	if err == nil {
		t.Fatal("run succeeded, want final evidence validation error")
	}
	for _, want := range []string{"validate final evidence at step 2", "final diagnosis requires evidence when trace exists"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestRuntimeDerivesLLMObservationsFromTraceStore(t *testing.T) {
	registry := tools.NewRegistry()
	store := trace.NewMemoryStore()
	store.Append(trace.Entry{
		Step:     1,
		ToolName: "http_check",
		Result: schema.Observation{
			Tool:    "http_check",
			Summary: "seeded observation from trace",
			Data: map[string]any{
				"status": 200,
			},
		},
	})
	provider := &captureProvider{
		actions: []schema.Action{
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "seeded observation is enough",
				Final: &schema.Diagnosis{
					Summary: "done",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: "http_check", Summary: "seeded observation from trace"},
					},
				},
			},
		},
	}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}), store)

	if _, err := runtime.Run(context.Background(), "check existing trace"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(provider.requests))
	}
	observations := provider.requests[0].Observations
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	if observations[0].Summary != "seeded observation from trace" {
		t.Fatalf("observation summary = %q", observations[0].Summary)
	}
}

func TestRuntimeIncludesInitialObservationsInLLMRequest(t *testing.T) {
	registry := tools.NewRegistry()
	store := trace.NewMemoryStore()
	provider := &captureProvider{
		actions: []schema.Action{
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "target context is enough",
				Final: &schema.Diagnosis{
					Summary: "done",
				},
			},
		},
	}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    1,
		ToolTimeout: time.Second,
		InitialObservations: []schema.Observation{
			{
				Tool:    "target_context",
				Summary: "configured chat_proj targets",
				Data: map[string]any{
					"backend_base_url": "http://localhost:8080",
				},
			},
		},
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}), store)

	if _, err := runtime.Run(context.Background(), "check configured target"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(provider.requests))
	}
	observations := provider.requests[0].Observations
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want initial observation", len(observations))
	}
	if observations[0].Tool != "target_context" {
		t.Fatalf("observation tool = %q, want target_context", observations[0].Tool)
	}
	if observations[0].Data["backend_base_url"] != "http://localhost:8080" {
		t.Fatalf("backend target = %#v", observations[0].Data["backend_base_url"])
	}
}

func TestRuntimeStopsAtMaxSteps(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(runtimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := trace.NewMemoryStore()
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type:           "tool_call",
			ThoughtSummary: "check once",
			Tool:           "http_check",
			Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
		},
		{
			Type:           "tool_call",
			ThoughtSummary: "check twice",
			Tool:           "http_check",
			Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
		},
	})
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      1,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
	}), store)

	_, err := runtime.Run(context.Background(), "keep checking")
	if err == nil {
		t.Fatal("expected max steps error")
	}
	if !strings.Contains(err.Error(), "max steps") {
		t.Fatalf("error = %q, want max steps", err.Error())
	}
}

func TestRuntimeWrapsProviderErrorWithStepContext(t *testing.T) {
	registry := tools.NewRegistry()
	store := trace.NewMemoryStore()
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}, failingProvider{err: errors.New("model returned invalid content")}, registry, policy.NewValidator(policy.Config{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}), store)

	_, err := runtime.Run(context.Background(), "check service")
	if err == nil {
		t.Fatal("run succeeded, want provider error")
	}
	for _, want := range []string{"plan next action at step 1", "model returned invalid content"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestRuntimeWrapsPolicyErrorWithStepContext(t *testing.T) {
	registry := tools.NewRegistry()
	store := trace.NewMemoryStore()
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type: "unknown",
		},
	})
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}), store)

	_, err := runtime.Run(context.Background(), "check service")
	if err == nil {
		t.Fatal("run succeeded, want policy error")
	}
	for _, want := range []string{"validate action at step 1", `unsupported action type "unknown"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestRuntimeContinuesAfterToolErrorAndPassesFailureObservation(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(failingRuntimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := trace.NewMemoryStore()
	provider := &captureProvider{
		actions: []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "check backend health first",
				Tool:           "http_check",
				Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "tool failure evidence is enough",
				Final: &schema.Diagnosis{
					Summary: "backend health check failed, use the tool error as evidence",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: "http_check", Summary: "connection refused"},
					},
				},
			},
		},
	}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      2,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
	}), store)

	diagnosis, err := runtime.Run(context.Background(), "check service")
	if err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if diagnosis.Summary != "backend health check failed, use the tool error as evidence" {
		t.Fatalf("summary = %q", diagnosis.Summary)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	observations := provider.requests[1].Observations
	if len(observations) != 1 {
		t.Fatalf("second request observations = %d, want 1", len(observations))
	}
	if observations[0].Tool != "http_check" {
		t.Fatalf("observation tool = %q, want http_check", observations[0].Tool)
	}
	if !strings.Contains(observations[0].Error, "connection refused") {
		t.Fatalf("observation error = %q, want tool error", observations[0].Error)
	}

	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("trace entries = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Error, "connection refused") {
		t.Fatalf("trace error = %q, want tool error", entries[0].Error)
	}
	if !strings.Contains(entries[0].Result.Error, "connection refused") {
		t.Fatalf("trace result error = %q, want tool error", entries[0].Result.Error)
	}
}

type captureProvider struct {
	actions  []schema.Action
	requests []llm.Request
	index    int
}

func (p *captureProvider) NextAction(ctx context.Context, request llm.Request) (schema.Action, error) {
	p.requests = append(p.requests, request)
	if p.index >= len(p.actions) {
		return schema.Action{}, nil
	}
	action := p.actions[p.index]
	p.index++
	return action, nil
}

type failingProvider struct {
	err error
}

func (p failingProvider) NextAction(context.Context, llm.Request) (schema.Action, error) {
	return schema.Action{}, p.err
}

type failingRuntimeTool struct{}

func (failingRuntimeTool) Name() string { return "http_check" }

func (failingRuntimeTool) Description() string { return "check HTTP endpoint" }

func (failingRuntimeTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{
		Properties: map[string]tools.ArgSpec{
			"url": {Type: "string", Required: true},
		},
	}
}

func (failingRuntimeTool) Run(context.Context, json.RawMessage) (schema.Observation, error) {
	return schema.Observation{
		Tool:    "http_check",
		Summary: "backend check failed",
	}, errors.New("connection refused")
}
