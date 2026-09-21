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
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

type runtimeTool struct{}

func (runtimeTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "http_check", Description: "check HTTP endpoint", Schema: tools.ToolSchema{
		Properties: map[string]tools.ArgSpec{
			"url": {Type: "string", Required: true},
		},
	}}
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
	store := new(trace.MemoryStore)
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
				Summary:   "backend is alive",
				RootCause: &schema.RootCause{Status: "undetermined"},
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
		ToolAllowlist: []string{"http_check"},
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
	store := new(trace.MemoryStore)
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
		ToolAllowlist: []string{"http_check"},
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

func TestRuntimeAllowsFlexibleSummaryFormatting(t *testing.T) {
	provider := &captureProvider{actions: []schema.Action{{
		Type: schema.ActionTypeFinal,
		Final: &schema.Diagnosis{
			Summary: "历史请求和当前复现现象相同，但根因证据不足。",
		},
	}}}
	runtime := NewRuntime(RuntimeConfig{MaxSteps: 1}, provider, tools.NewRegistry(), policy.NewValidator(policy.Config{}), new(trace.MemoryStore))

	diagnosis, err := runtime.Run(context.Background(), "最终按 [历史]、[结论] 两段输出 Summary")
	if err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if diagnosis.Summary != "历史请求和当前复现现象相同，但根因证据不足。" {
		t.Fatalf("summary = %q", diagnosis.Summary)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("decision requests = %d, want no formatting retry", len(provider.requests))
	}
}

func TestRuntimeDerivesLLMObservationsFromTraceStore(t *testing.T) {
	registry := tools.NewRegistry()
	store := new(trace.MemoryStore)
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
					Summary:   "done",
					RootCause: &schema.RootCause{Status: "undetermined"},
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
	}, provider, registry, policy.NewValidator(policy.Config{}), store)

	if _, err := runtime.Run(context.Background(), "check existing trace"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(provider.requests))
	}
	if len(provider.planRequests) != 0 {
		t.Fatalf("planning requests = %d, want none for direct action", len(provider.planRequests))
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
	store := new(trace.MemoryStore)
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
	}, provider, registry, policy.NewValidator(policy.Config{}), store)

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
	}, provider, tools.NewRegistry(), policy.NewValidator(policy.Config{}), new(trace.MemoryStore))

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

func TestRuntimeSetsPlanWithoutConsumingStep(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(runtimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := new(trace.MemoryStore)
	provider := &captureProvider{
		needsPlanAt: map[int]bool{0: true},
		plans: []*schema.Plan{
			{
				Reason: "复杂目标需要先列检查项",
				Items: []schema.PlanItem{
					{ID: "backend", Goal: "检查后端服务是否存活", Status: "pending"},
				},
			},
		},
		actions: []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "检查后端",
				PlanItemID:     "backend",
				Tool:           "http_check",
				Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "计划项已有证据",
				Final: &schema.Diagnosis{
					Summary:   "后端服务存活",
					RootCause: &schema.RootCause{Status: "undetermined"},
					Evidence: []schema.Evidence{
						{Step: 1, Tool: "http_check", Summary: "backend returned 200"},
					},
					Coverage: []schema.CoverageItem{
						{
							PlanItemID: "backend",
							Status:     "done",
							Evidence: []schema.Evidence{
								{Step: 1, Tool: "http_check", Summary: "backend returned 200"},
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
		ToolAllowlist: []string{"http_check"},
		ToolSchemas: map[string]tools.ToolSchema{
			"http_check": runtimeTool{}.Spec().Schema,
		},
	}), store)

	if _, err := runtime.Run(context.Background(), "检查后端"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.planRequests) != 1 || len(provider.requests) != 3 {
		t.Fatalf("plan/decision requests = %d/%d, want 1/3", len(provider.planRequests), len(provider.requests))
	}
	if provider.planRequests[0].Plan != nil {
		t.Fatalf("first planning request plan = %#v, want nil", provider.planRequests[0].Plan)
	}
	if provider.requests[0].Plan != nil {
		t.Fatalf("planning decision request plan = %#v, want nil", provider.requests[0].Plan)
	}
	if provider.requests[1].Plan == nil || provider.requests[1].Plan.Items[0].ID != "backend" {
		t.Fatalf("first action request plan = %#v, want backend plan", provider.requests[1].Plan)
	}
	if !provider.requests[0].PlanningAllowed || provider.requests[1].PlanningAllowed || !provider.requests[2].PlanningAllowed {
		t.Fatalf("planning_allowed = %v/%v/%v, want true/false/true", provider.requests[0].PlanningAllowed, provider.requests[1].PlanningAllowed, provider.requests[2].PlanningAllowed)
	}
	if provider.requests[0].Step != 1 || provider.requests[1].Step != 1 || provider.requests[2].Step != 2 {
		t.Fatalf("decision request steps = %d/%d/%d, want 1/1/2", provider.requests[0].Step, provider.requests[1].Step, provider.requests[2].Step)
	}
	if len(store.List()) != 2 || store.List()[0].Step != 1 || store.List()[0].PlanItemID != "backend" || store.List()[1].ActionType != schema.ActionTypeFinal {
		t.Fatalf("trace = %#v, want tool/final actions", store.List())
	}
	if runtime.Plan().Items[0].Goal != "检查后端服务是否存活" || runtime.Plan().Items[0].Status != "done" {
		t.Fatalf("runtime plan = %#v", runtime.Plan())
	}
}

func TestRuntimeRejectsDuplicateSuccessfulToolCall(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(runtimeTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	action := schema.Action{
		Type: schema.ActionTypeToolCall,
		Tool: "http_check",
		Args: json.RawMessage(`{"url":"http://localhost:8080/health"}`),
	}
	provider := &captureProvider{actions: []schema.Action{
		action,
		action,
		{
			Type: schema.ActionTypeFinal,
			Final: &schema.Diagnosis{
				Summary:   "已有检查结果，无需重复调用。",
				RootCause: &schema.RootCause{Status: "undetermined"},
				Evidence:  []schema.Evidence{{Step: 1, Tool: "http_check"}},
			},
		},
	}}
	store := new(trace.MemoryStore)
	runtime := NewRuntime(RuntimeConfig{MaxSteps: 2}, provider, registry, policy.NewValidator(policy.Config{
		ToolAllowlist: []string{"http_check"},
	}), store)

	if _, err := runtime.Run(context.Background(), "检查一次后端健康状态"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if len(provider.requests) != 3 || !strings.Contains(provider.requests[2].Correction, "duplicate successful tool call") {
		t.Fatalf("requests/correction = %d/%q", len(provider.requests), provider.requests[2].Correction)
	}
	entries := store.List()
	if len(entries) != 2 || entries[0].ToolName != "http_check" || entries[1].ActionType != schema.ActionTypeFinal {
		t.Fatalf("trace = %#v, want one tool call and final", entries)
	}
}

func TestRuntimeAppliesToolArgOverridesAndRedactsTraceArgs(t *testing.T) {
	seenDSN := ""
	tool := dsnRuntimeTool{seen: &seenDSN}
	registry := tools.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	store := new(trace.MemoryStore)
	provider := llm.NewMockProvider([]schema.Action{
		{
			Type:           schema.ActionTypeToolCall,
			ThoughtSummary: "check configured postgres",
			Tool:           tool.Spec().Name,
			Args:           json.RawMessage(`{}`),
		},
		{
			Type:           schema.ActionTypeFinal,
			ThoughtSummary: "postgres check is enough",
			Final: &schema.Diagnosis{
				Summary:   "done",
				RootCause: &schema.RootCause{Status: "undetermined"},
				Evidence: []schema.Evidence{
					{Step: 1, Tool: tool.Spec().Name, Summary: "postgres checked"},
				},
			},
		},
	})
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    2,
		ToolTimeout: time.Second,
		ToolArgOverrides: map[string]map[string]any{
			tool.Spec().Name: {
				"dsn": "postgres://app:secret@db.local:5432/chat_proj",
			},
		},
	}, provider, registry, policy.NewValidator(policy.Config{
		ToolSchemas: map[string]tools.ToolSchema{
			tool.Spec().Name: tool.Spec().Schema,
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
	store := new(trace.MemoryStore)
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
					Summary:   "done",
					RootCause: &schema.RootCause{Status: "undetermined"},
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
	}, provider, registry, policy.NewValidator(policy.Config{}), store)

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

func TestNextStepUsesLastTraceStep(t *testing.T) {
	entries := []trace.Entry{
		{Step: 1},
		{Step: 9},
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
	store := new(trace.MemoryStore)
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
		ToolAllowlist: []string{"http_check"},
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
	store := new(trace.MemoryStore)
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    1,
		ToolTimeout: time.Second,
	}, failingProvider{err: errors.New("model returned invalid content")}, registry, policy.NewValidator(policy.Config{}), store)

	_, err := runtime.Run(context.Background(), "check service")
	if err == nil {
		t.Fatal("run succeeded, want provider error")
	}
	for _, want := range []string{"request next decision at step 1", "model returned invalid content"} {
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
	}, provider, tools.NewRegistry(), policy.NewValidator(policy.Config{}), new(trace.MemoryStore))

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
	store := new(trace.MemoryStore)
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
	}, provider, registry, policy.NewValidator(policy.Config{}), store)

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
	store := new(trace.MemoryStore)
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
	store := new(trace.MemoryStore)
	provider := &captureProvider{
		needsPlanAt: map[int]bool{0: true, 2: true},
		plans: []*schema.Plan{
			{
				Reason: "先检查后端",
				Items:  []schema.PlanItem{{ID: "backend", Goal: "检查后端服务", Status: "pending"}},
			},
			nil,
		},
		actions: []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "check backend health first",
				PlanItemID:     "backend",
				Tool:           "http_check",
				Args:           json.RawMessage(`{"url":"http://localhost:8080/health"}`),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "tool failure evidence is enough",
				Final: &schema.Diagnosis{
					Summary:   "backend health check failed, use the tool error as evidence",
					RootCause: &schema.RootCause{Status: "undetermined"},
					Evidence: []schema.Evidence{
						{Step: 1, Tool: "http_check", Summary: "connection refused"},
					},
					Coverage: []schema.CoverageItem{
						{
							PlanItemID: "backend",
							Status:     "blocked",
							Evidence: []schema.Evidence{
								{Step: 1, Tool: "http_check", Summary: "connection refused"},
							},
						},
					},
				},
			},
		},
	}
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    2,
		ToolTimeout: time.Second,
	}, provider, registry, policy.NewValidator(policy.Config{
		ToolAllowlist: []string{"http_check"},
	}), store)

	diagnosis, err := runtime.Run(context.Background(), "check service")
	if err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if diagnosis.Summary != "backend health check failed, use the tool error as evidence" {
		t.Fatalf("summary = %q", diagnosis.Summary)
	}

	if len(provider.requests) != 4 {
		t.Fatalf("provider requests = %d, want 4", len(provider.requests))
	}
	if len(provider.planRequests) != 2 || len(provider.planRequests[1].Observations) != 1 {
		t.Fatalf("planning requests = %#v, want requested plan and replan after tool error", provider.planRequests)
	}
	observations := provider.requests[2].Observations
	if len(observations) != 1 {
		t.Fatalf("replan decision observations = %d, want 1", len(observations))
	}
	if observations[0].Tool != "http_check" {
		t.Fatalf("observation tool = %q, want http_check", observations[0].Tool)
	}
	if observations[0].PlanItemID != "backend" {
		t.Fatalf("observation plan item = %q, want backend", observations[0].PlanItemID)
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
	if entries[0].PlanItemID != "backend" {
		t.Fatalf("trace plan item = %q, want backend", entries[0].PlanItemID)
	}
}

func TestRuntimeCheckpointsCallsBeforeAndAfterExternalExecution(t *testing.T) {
	registry := tools.NewRegistry()
	var checkpoints []Checkpoint
	sawRunningTool := false
	tool := checkpointRuntimeTool{onRun: func() {
		for _, checkpoint := range checkpoints {
			if len(checkpoint.Calls) == 0 {
				continue
			}
			last := checkpoint.Calls[len(checkpoint.Calls)-1]
			if last.Kind == runstore.CallKindTool && last.Status == runstore.CallStatusRunning && last.Result == nil {
				sawRunningTool = true
			}
		}
	}}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	provider := llm.NewMockProvider([]schema.Action{
		{Type: schema.ActionTypeToolCall, Tool: tool.Spec().Name, Args: json.RawMessage(`{"url":"http://localhost/health"}`)},
		{
			Type: schema.ActionTypeFinal,
			Final: &schema.Diagnosis{
				Summary:   "tool result is recorded",
				RootCause: &schema.RootCause{Status: "undetermined"},
				Evidence:  []schema.Evidence{{Step: 1, Tool: tool.Spec().Name, Summary: "backend returned 200"}},
			},
		},
	})
	store := new(trace.MemoryStore)
	runtime := NewRuntime(RuntimeConfig{
		MaxSteps:    2,
		ToolTimeout: time.Second,
		Checkpoint: func(checkpoint Checkpoint) error {
			checkpoint.Calls = append([]runstore.Call(nil), checkpoint.Calls...)
			checkpoint.Trace = append([]trace.Entry(nil), checkpoint.Trace...)
			checkpoints = append(checkpoints, checkpoint)
			return nil
		},
	}, provider, registry, policy.NewValidator(policy.Config{ToolAllowlist: []string{tool.Spec().Name}}), store)

	if _, err := runtime.Run(context.Background(), "check checkpoint lifecycle"); err != nil {
		t.Fatalf("run runtime: %v", err)
	}
	if !sawRunningTool {
		t.Fatalf("checkpoints = %#v, want tool running checkpoint before tool.Run", checkpoints)
	}
	calls := runtime.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %#v, want two LLM decisions and one tool call", calls)
	}
	toolCall := calls[1]
	if toolCall.Kind != runstore.CallKindTool || toolCall.Status != runstore.CallStatusSucceeded || toolCall.Result == nil || toolCall.Result.Summary != "backend returned 200" {
		t.Fatalf("tool call = %#v", toolCall)
	}
	entries := store.List()
	if entries[0].CallID != toolCall.CallID || entries[1].CallID == "" {
		t.Fatalf("trace call ids = %#v, want tool and final call links", entries)
	}
}

func TestRuntimeBoundsRetriesByErrorClass(t *testing.T) {
	transient := &retryProvider{remainingTransientFailures: 1}
	runtime := NewRuntime(RuntimeConfig{MaxSteps: 1}, transient, tools.NewRegistry(), policy.NewValidator(policy.Config{}), new(trace.MemoryStore))
	if _, err := runtime.Run(context.Background(), "retry temporary provider error"); err != nil {
		t.Fatalf("run transient provider: %v", err)
	}
	transientCalls := runtime.Calls()
	if transient.calls != 2 || len(transientCalls) != 2 || transientCalls[0].ErrorClass != runstore.ErrorClassTransient || transientCalls[1].Status != runstore.CallStatusSucceeded {
		t.Fatalf("transient calls = %#v / provider=%d", transientCalls, transient.calls)
	}
	httpTransient := &retryProvider{remainingHTTPFailures: 1}
	runtime = NewRuntime(RuntimeConfig{MaxSteps: 1}, httpTransient, tools.NewRegistry(), policy.NewValidator(policy.Config{}), new(trace.MemoryStore))
	if _, err := runtime.Run(context.Background(), "retry provider 503"); err != nil {
		t.Fatalf("run transient HTTP provider: %v", err)
	}
	if httpTransient.calls != 2 || runtime.Calls()[0].ErrorClass != runstore.ErrorClassTransient {
		t.Fatalf("HTTP transient calls = %#v / provider=%d", runtime.Calls(), httpTransient.calls)
	}

	permanent := &retryProvider{permanentErr: errors.New("invalid credentials")}
	runtime = NewRuntime(RuntimeConfig{MaxSteps: 1}, permanent, tools.NewRegistry(), policy.NewValidator(policy.Config{}), new(trace.MemoryStore))
	if _, err := runtime.Run(context.Background(), "do not retry permanent provider error"); err == nil {
		t.Fatal("permanent provider failure succeeded")
	}
	permanentCalls := runtime.Calls()
	if permanent.calls != 1 || len(permanentCalls) != 1 || permanentCalls[0].ErrorClass != runstore.ErrorClassPermanent {
		t.Fatalf("permanent calls = %#v / provider=%d", permanentCalls, permanent.calls)
	}
}

func TestRuntimeMarksCancelledSideEffectToolUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := tools.NewRegistry()
	tool := cancellingRuntimeTool{cancel: cancel}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	provider := llm.NewMockProvider([]schema.Action{{Type: schema.ActionTypeToolCall, Tool: tool.Spec().Name, Args: json.RawMessage(`{}`)}})
	runtime := NewRuntime(RuntimeConfig{MaxSteps: 2}, provider, registry, policy.NewValidator(policy.Config{ToolAllowlist: []string{tool.Spec().Name}}), new(trace.MemoryStore))

	_, err := runtime.Run(ctx, "cancel side-effect tool")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context cancelled", err)
	}
	calls := runtime.Calls()
	if len(calls) != 2 || calls[1].Status != runstore.CallStatusUnknown || calls[1].ErrorClass != runstore.ErrorClassUnknown || !runstore.HasUnknownSideEffect(calls) {
		t.Fatalf("calls = %#v, want unknown side-effect tool call", calls)
	}
}

type captureProvider struct {
	plans        []*schema.Plan
	actions      []schema.Action
	needsPlanAt  map[int]bool
	planRequests []llm.Request
	requests     []llm.Request
	planIndex    int
	index        int
}

type checkpointRuntimeTool struct {
	onRun func()
}

func (t checkpointRuntimeTool) Spec() tools.ToolSpec {
	return runtimeTool{}.Spec()
}

func (t checkpointRuntimeTool) Run(ctx context.Context, args json.RawMessage) (schema.Observation, error) {
	if t.onRun != nil {
		t.onRun()
	}
	return runtimeTool{}.Run(ctx, args)
}

type retryProvider struct {
	remainingTransientFailures int
	remainingHTTPFailures      int
	permanentErr               error
	calls                      int
}

func (p *retryProvider) Plan(context.Context, llm.Request) (*schema.Plan, error) {
	return nil, nil
}

func (p *retryProvider) Next(context.Context, llm.Request) (llm.Decision, error) {
	p.calls++
	if p.remainingTransientFailures > 0 {
		p.remainingTransientFailures--
		return llm.Decision{}, temporaryRuntimeError{}
	}
	if p.remainingHTTPFailures > 0 {
		p.remainingHTTPFailures--
		return llm.Decision{}, errors.New("provider request failed: status 503")
	}
	if p.permanentErr != nil {
		return llm.Decision{}, p.permanentErr
	}
	return llm.Decision{Action: &schema.Action{Type: schema.ActionTypeFinal, Final: &schema.Diagnosis{Summary: "retry succeeded"}}}, nil
}

type temporaryRuntimeError struct{}

func (temporaryRuntimeError) Error() string   { return "temporary provider transport failure" }
func (temporaryRuntimeError) Timeout() bool   { return false }
func (temporaryRuntimeError) Temporary() bool { return true }

type cancellingRuntimeTool struct {
	cancel context.CancelFunc
}

func (t cancellingRuntimeTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "effect_tool", Description: "side-effect test tool", Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{}}, SideEffect: true}
}

func (t cancellingRuntimeTool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	t.cancel()
	<-ctx.Done()
	return schema.Observation{Tool: t.Spec().Name, Summary: "effect tool interrupted"}, ctx.Err()
}

func (p *captureProvider) Plan(ctx context.Context, request llm.Request) (*schema.Plan, error) {
	p.planRequests = append(p.planRequests, request)
	if p.planIndex >= len(p.plans) {
		return nil, nil
	}
	plan := p.plans[p.planIndex]
	p.planIndex++
	return plan, nil
}

func (p *captureProvider) Next(ctx context.Context, request llm.Request) (llm.Decision, error) {
	call := len(p.requests)
	p.requests = append(p.requests, request)
	if p.needsPlanAt[call] {
		return llm.Decision{NeedsPlan: true}, nil
	}
	if p.index >= len(p.actions) {
		action := schema.Action{}
		return llm.Decision{Action: &action}, nil
	}
	action := p.actions[p.index]
	p.index++
	return llm.Decision{Action: &action}, nil
}

type failingProvider struct {
	err error
}

type timeoutProvider struct {
	calls int
}

func (p *timeoutProvider) Plan(context.Context, llm.Request) (*schema.Plan, error) {
	return nil, nil
}

func (p *timeoutProvider) Next(ctx context.Context, _ llm.Request) (llm.Decision, error) {
	p.calls++
	<-ctx.Done()
	return llm.Decision{}, ctx.Err()
}

func (p failingProvider) Plan(context.Context, llm.Request) (*schema.Plan, error) {
	return nil, nil
}

func (p failingProvider) Next(context.Context, llm.Request) (llm.Decision, error) {
	return llm.Decision{}, p.err
}

type failingRuntimeTool struct{}

type dsnRuntimeTool struct {
	seen *string
}

func (t dsnRuntimeTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "postgres_ping", Description: "check postgres", Schema: tools.ToolSchema{
		Properties: map[string]tools.ArgSpec{
			"dsn": {Type: "string", Required: true},
		},
	}}
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
	return schema.Observation{Tool: t.Spec().Name, Summary: "postgres checked"}, nil
}

func (failingRuntimeTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "http_check", Description: "check HTTP endpoint", Schema: tools.ToolSchema{
		Properties: map[string]tools.ArgSpec{
			"url": {Type: "string", Required: true},
		},
	}}
}

func (failingRuntimeTool) Run(context.Context, json.RawMessage) (schema.Observation, error) {
	return schema.Observation{
		Tool:    "http_check",
		Summary: "backend check failed",
	}, errors.New("connection refused")
}
