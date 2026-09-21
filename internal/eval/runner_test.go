package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestRunMockBuiltinScenariosAreDeterministicAndOffline(t *testing.T) {
	for _, scenario := range BuiltinScenarios() {
		scenario := scenario
		t.Run(scenario.ID, func(t *testing.T) {
			result, err := RunMock(context.Background(), scenario)
			if err != nil {
				t.Fatalf("run mock scenario: %v", err)
			}
			if result.Status != StatusPassed {
				t.Fatalf("status = %q, want %q; assertions = %#v", result.Status, StatusPassed, result.Assertions)
			}
			if !AllPassed(result.Assertions) {
				t.Fatalf("assertions did not all pass: %#v", result.Assertions)
			}
			if result.Mode != ModeMock || result.Model != "mock" || result.ExecutedRealModel {
				t.Fatalf("mock metadata = %#v", result)
			}
			if result.RunID != "" {
				t.Fatalf("mock run id = %q, want empty", result.RunID)
			}
			if result.ScenarioID != scenario.ID || result.ScenarioVersion != scenario.Version {
				t.Fatalf("scenario metadata = %#v", result)
			}
			if result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
				t.Fatalf("timestamps = %s/%s", result.StartedAt, result.FinishedAt)
			}
			if result.Command != "eval mock --scenario "+scenario.ID {
				t.Fatalf("command = %q", result.Command)
			}

			wantTraceSteps := 1
			if scenario.ID == ScenarioLogin500 {
				wantTraceSteps = 3
			}
			if result.TraceSteps != wantTraceSteps {
				t.Fatalf("trace steps = %d, want %d", result.TraceSteps, wantTraceSteps)
			}
		})
	}
}

func TestLogin500ScenarioUsesInMemoryFixtureTools(t *testing.T) {
	scenario, ok := LookupScenario(ScenarioLogin500)
	if !ok {
		t.Fatal("login-500 scenario not found")
	}
	registry, _, err := mockRegistryAndValidator(scenario)
	if err != nil {
		t.Fatalf("build fixture registry: %v", err)
	}

	for _, want := range []struct {
		name    string
		summary string
	}{
		{name: "http_check", summary: "returned HTTP 500"},
		{name: "log_read", summary: `read 1 log lines matching "ERROR"`},
	} {
		tool, exists := registry.Get(want.name)
		if !exists {
			t.Fatalf("fixture tool %q not registered", want.name)
		}
		observation, err := tool.Run(context.Background(), nil)
		if err != nil {
			t.Fatalf("run fixture %q: %v", want.name, err)
		}
		if observation.Tool != want.name || observation.Summary != want.summary {
			t.Fatalf("fixture observation = %#v, want %q/%q", observation, want.name, want.summary)
		}
	}
}

func TestRunMockReportsExpectationFailureWithoutExecutionError(t *testing.T) {
	scenario, ok := LookupScenario(ScenarioSkeleton)
	if !ok {
		t.Fatal("skeleton scenario not found")
	}
	scenario.Expectations.SummaryContains = []string{"this text cannot be in the fixed final diagnosis"}

	result, err := RunMock(context.Background(), scenario)
	if err != nil {
		t.Fatalf("run mock scenario returned execution error: %v", err)
	}
	if result.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.Error != "" {
		t.Fatalf("result error = %q, want empty for assertion failure", result.Error)
	}
	if AllPassed(result.Assertions) {
		t.Fatalf("assertions = %#v, want a failure", result.Assertions)
	}
}

func TestRunMockReturnsPersistableFailedResultForInvalidScenario(t *testing.T) {
	scenario, ok := LookupScenario(ScenarioSkeleton)
	if !ok {
		t.Fatal("skeleton scenario not found")
	}
	scenario.Actions = nil

	result, err := RunMock(context.Background(), scenario)
	if err == nil {
		t.Fatal("run mock scenario succeeded, want validation error")
	}
	if result.Status != StatusFailed || !strings.Contains(result.Error, "requires at least one action") {
		t.Fatalf("failed result = %#v", result)
	}
	if result.ID == "" || result.FinishedAt.IsZero() {
		t.Fatalf("failed result is not persistable: %#v", result)
	}
}

func TestEvaluateScenarioIsReusableForExternalEvaluator(t *testing.T) {
	scenario, ok := LookupScenario(ScenarioLogin500)
	if !ok {
		t.Fatal("login-500 scenario not found")
	}
	diagnosis := &schema.Diagnosis{
		Summary: "登录接口返回 500，当前状态需要继续检查。",
	}
	entries := []trace.Entry{
		{Step: 1, ToolName: "http_check"},
		{Step: 2, ToolName: "log_read"},
	}
	assertions := EvaluateScenario(scenario, diagnosis, entries)
	if !AllPassed(assertions) {
		t.Fatalf("assertions = %#v, want all passed", assertions)
	}

	assertions = EvaluateScenario(scenario, diagnosis, entries[:1])
	if AllPassed(assertions) {
		t.Fatalf("assertions = %#v, want missing tool failure", assertions)
	}
	if assertionPassed(assertions, "required_tool:log_read") {
		t.Fatalf("assertions = %#v, log_read should be missing", assertions)
	}

	assertions = EvaluateScenario(scenario, diagnosis, []trace.Entry{
		{Step: 1, ToolName: "log_read"},
		{Step: 2, ToolName: "http_check"},
	})
	if assertionPassed(assertions, "tool_sequence") {
		t.Fatalf("assertions = %#v, reversed tool order should fail", assertions)
	}
}

func TestBuiltinScenariosReturnIndependentCopies(t *testing.T) {
	first := BuiltinScenarios()
	if len(first) == 0 {
		t.Fatal("no builtin scenarios")
	}
	first[0].Name = "mutated"
	first[0].Expectations.SummaryContains[0] = "mutated"

	second := BuiltinScenarios()
	if second[0].Name == "mutated" || second[0].Expectations.SummaryContains[0] == "mutated" {
		t.Fatalf("builtin scenarios were mutated: %#v", second[0])
	}
}

func assertionPassed(assertions []Assertion, name string) bool {
	for _, assertion := range assertions {
		if assertion.Name == name {
			return assertion.Passed
		}
	}
	return false
}
