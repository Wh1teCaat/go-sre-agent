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
	if len(entries) != 2 {
		t.Fatalf("trace entries = %d, want tool and final", len(entries))
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
		{
			Type:           schema.ActionTypeFinal,
			ThoughtSummary: "summary still without evidence",
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
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:    2,
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
	if observations[0].Step != 1 {
		t.Fatalf("observation step = %d, want trace step 1", observations[0].Step)
	}
}

func TestRuntimeIncludesTargetContextOutsideObservations(t *testing.T) {
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
		TargetContext: map[string]any{
			"backend_base_url": "http://localhost:8080",
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
	if got := provider.requests[0].TargetContext["backend_base_url"]; got != "http://localhost:8080" {
		t.Fatalf("backend target context = %#v", got)
	}
	if len(provider.requests[0].Observations) != 0 {
		t.Fatalf("observations = %#v, want only real tool observations", provider.requests[0].Observations)
	}
}

func TestRuntimePassesMemoryHintsOutsideObservations(t *testing.T) {
	provider := &captureProvider{actions: []schema.Action{{
		Type:  schema.ActionTypeFinal,
		Final: &schema.Diagnosis{Summary: "done"},
	}}}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps: 1,
		Memories: []schema.Memory{{
			Subject:     "历史登录故障",
			Content:     "曾发现 users 表缺失",
			SourceRunID: "run_old",
		}},
	}, provider, tools.NewRegistry(), policy.NewValidator(policy.Config{}), trace.NewMemoryStore())

	if _, err := runtime.Run(context.Background(), "诊断当前登录故障"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	request := provider.requests[0]
	if len(request.Memories) != 1 || request.Memories[0].SourceRunID != "run_old" {
		t.Fatalf("memories = %#v", request.Memories)
	}
	if len(request.Observations) != 0 {
		t.Fatalf("observations = %#v, want no historical evidence", request.Observations)
	}
}

func TestRuntimeMergesPlanAndPassesItToNextStep(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(runtimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := trace.NewMemoryStore()
	provider := &captureProvider{
		actions: []schema.Action{
			{
				Type:           schema.ActionTypePlan,
				ThoughtSummary: "先规划覆盖面",
				Plan: &schema.Plan{
					Reason: "复杂目标需要先列检查项",
					Items: []schema.PlanItem{
						{ID: "backend", Goal: "检查后端服务是否存活", Status: "pending"},
					},
				},
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "检查后端",
				Tool:           "http_check",
				Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "计划项已有证据",
				Final: &schema.Diagnosis{
					Summary: "后端服务存活",
					Evidence: []schema.Evidence{
						{Step: 2, Tool: "http_check", Summary: "backend returned 200"},
					},
					Coverage: []schema.CoverageItem{
						{
							PlanItemID: "backend",
							Status:     "done",
							Evidence: []schema.Evidence{
								{Step: 2, Tool: "http_check", Summary: "backend returned 200"},
							},
						},
					},
				},
			},
		},
	}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
		ToolSchemas: map[string]tools.ToolSchema{
			"http_check": runtimeTool{}.Schema(),
		},
	}), store)

	if _, err := runtime.Run(context.Background(), "检查后端"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(provider.requests))
	}
	if provider.requests[0].Plan != nil {
		t.Fatalf("first request plan = %#v, want nil", provider.requests[0].Plan)
	}
	if provider.requests[1].Plan == nil || provider.requests[1].Plan.Items[0].ID != "backend" {
		t.Fatalf("second request plan = %#v, want backend plan", provider.requests[1].Plan)
	}
	if len(store.List()) != 3 || store.List()[0].ActionType != schema.ActionTypePlan || store.List()[1].Step != 2 || store.List()[2].ActionType != schema.ActionTypeFinal {
		t.Fatalf("trace = %#v, want plan/tool/final actions", store.List())
	}
	if runtime.Plan().Items[0].Goal != "检查后端服务是否存活" {
		t.Fatalf("runtime plan = %#v", runtime.Plan())
	}
}

func TestMergePlanKeepsExistingItemsDuringReplan(t *testing.T) {
	current := schema.Plan{
		Reason: "初始计划",
		Items: []schema.PlanItem{
			{ID: "backend", Goal: "检查后端", Status: "pending"},
			{ID: "logs", Goal: "检查日志", Status: "pending"},
		},
	}
	next := schema.Plan{
		Reason: "后端正常，补查 Redis",
		Items: []schema.PlanItem{
			{ID: "backend", Goal: "检查后端", Status: "done"},
			{ID: "redis", Goal: "检查 Redis", Status: "pending"},
		},
	}

	merged := mergePlan(current, next)
	if merged.Reason != next.Reason || len(merged.Items) != 3 {
		t.Fatalf("merged plan = %#v", merged)
	}
	if merged.Items[0].Status != "done" || merged.Items[1].ID != "logs" || merged.Items[2].ID != "redis" {
		t.Fatalf("merged items = %#v", merged.Items)
	}
}

func TestRuntimeAppliesToolArgOverridesAndRedactsTraceArgs(t *testing.T) {
	seenDSN := ""
	tool := dsnRuntimeTool{seen: &seenDSN}
	registry := tools.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := trace.NewMemoryStore()
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type:           schema.ActionTypeToolCall,
			ThoughtSummary: "check configured postgres",
			Tool:           tool.Name(),
			Args:           json.RawMessage(`{}`),
		},
		{
			Type:           schema.ActionTypeFinal,
			ThoughtSummary: "postgres check is enough",
			Final: &schema.Diagnosis{
				Summary: "done",
				Evidence: []schema.Evidence{
					{Step: 1, Tool: tool.Name(), Summary: "postgres checked"},
				},
			},
		},
	})
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    2,
		ToolTimeout: time.Second,
		ToolArgOverrides: map[string]map[string]any{
			tool.Name(): {
				"dsn": "postgres://app:secret@db.local:5432/chat_proj",
			},
		},
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps: 2,
		ToolSchemas: map[string]tools.ToolSchema{
			tool.Name(): tool.Schema(),
		},
	}), store)

	if _, err := runtime.Run(context.Background(), "check postgres"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if seenDSN != "postgres://app:secret@db.local:5432/chat_proj" {
		t.Fatalf("tool saw dsn = %q", seenDSN)
	}
	entries := store.List()
	if len(entries) != 2 {
		t.Fatalf("trace entries = %d, want tool and final", len(entries))
	}
	if entries[0].Args["dsn"] != "[REDACTED]" {
		t.Fatalf("trace dsn arg = %#v, want redacted", entries[0].Args["dsn"])
	}
}

func TestRuntimeContinuesStepNumberAfterExistingTrace(t *testing.T) {
	registry := tools.NewRegistry()
	store := trace.NewMemoryStore()
	store.Append(trace.Entry{
		Step:     1,
		ToolName: "http_check",
		Result: schema.Observation{
			Tool:    "http_check",
			Summary: "backend returned 200",
		},
	})
	provider := &captureProvider{
		actions: []schema.Action{
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "existing evidence is enough",
				Final: &schema.Diagnosis{
					Summary: "done",
					Evidence: []schema.Evidence{
						{Step: 1, Tool: "http_check", Summary: "backend returned 200"},
					},
				},
			},
		},
	}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		MaxSteps:    3,
		ToolTimeout: time.Second,
	}), store)

	if _, err := runtime.Run(context.Background(), "resume existing trace"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(provider.requests))
	}
	if provider.requests[0].Step != 2 {
		t.Fatalf("request step = %d, want 2", provider.requests[0].Step)
	}
}

func TestNextStepUsesMaxTraceStep(t *testing.T) {
	entries := []trace.Entry{
		{Step: 9},
		{Step: 1},
	}

	if got := nextStep(entries); got != 10 {
		t.Fatalf("next step = %d, want 10", got)
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

func TestRuntimeAppliesLLMTimeoutAndRetriesOnce(t *testing.T) {
	provider := &timeoutProvider{}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:   1,
		LLMTimeout: 10 * time.Millisecond,
	}, provider, tools.NewRegistry(), policy.NewValidator(policy.Config{}), trace.NewMemoryStore())

	_, err := runtime.Run(context.Background(), "检查模型超时")
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want one retry", provider.calls)
	}
}

func TestRuntimeWrapsPolicyErrorWithStepContext(t *testing.T) {
	registry := tools.NewRegistry()
	store := trace.NewMemoryStore()
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type: "unknown",
		},
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

func TestRuntimeRetriesInvalidActionWithCorrection(t *testing.T) {
	provider := &captureProvider{actions: []schema.Action{
		{Type: "unknown"},
		{Type: schema.ActionTypeFinal, Final: &schema.Diagnosis{Summary: "纠错成功"}},
	}}
	store := trace.NewMemoryStore()
	runtime := NewRuntime(RuntimeConfig{MaxSteps: 1}, provider, tools.NewRegistry(), policy.NewValidator(policy.Config{}), store)

	diagnosis, err := runtime.Run(context.Background(), "验证 action 纠错")
	if err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if diagnosis.Summary != "纠错成功" || len(provider.requests) != 2 {
		t.Fatalf("diagnosis/requests = %#v/%d", diagnosis, len(provider.requests))
	}
	if !strings.Contains(provider.requests[1].Correction, "unsupported action type") {
		t.Fatalf("correction = %q", provider.requests[1].Correction)
	}
	entries := store.List()
	if len(entries) != 1 || entries[0].ActionType != schema.ActionTypeFinal || entries[0].LLMAttempts != 2 {
		t.Fatalf("trace = %#v", entries)
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
	if len(entries) != 2 {
		t.Fatalf("trace entries = %d, want tool and final", len(entries))
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

type timeoutProvider struct {
	calls int
}

func (p *timeoutProvider) NextAction(ctx context.Context, _ llm.Request) (schema.Action, error) {
	p.calls++
	<-ctx.Done()
	return schema.Action{}, ctx.Err()
}

func (p failingProvider) NextAction(context.Context, llm.Request) (schema.Action, error) {
	return schema.Action{}, p.err
}

type failingRuntimeTool struct{}

type dsnRuntimeTool struct {
	seen *string
}

func (t dsnRuntimeTool) Name() string { return "postgres_ping" }

func (t dsnRuntimeTool) Description() string { return "check postgres" }

func (t dsnRuntimeTool) Schema() tools.ToolSchema {
	return tools.ToolSchema{
		Properties: map[string]tools.ArgSpec{
			"dsn": {Type: "string", Required: true},
		},
	}
}

func (t dsnRuntimeTool) Run(_ context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args struct {
		DSN string `json:"dsn"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, err
	}
	if t.seen != nil {
		*t.seen = args.DSN
	}
	return schema.Observation{Tool: t.Name(), Summary: "postgres checked"}, nil
}

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
