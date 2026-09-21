package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

// RunMock evaluates one fixed scenario with an in-process MockProvider and
// ToolFixture implementations. It never reads LLM configuration and never
// opens a network connection. A failed expectation is represented by a failed
// Result with a nil error; a non-nil error means the runner could not execute
// the scenario at all. In both cases the returned Result is suitable for
// persistence.
func RunMock(ctx context.Context, scenario Scenario) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	startedAt := time.Now().UTC()
	result := Result{
		ID:                NewResultID(startedAt),
		ScenarioID:        scenario.ID,
		ScenarioName:      scenario.Name,
		ScenarioVersion:   scenario.Version,
		Command:           "eval mock --scenario " + scenario.ID,
		Mode:              ModeMock,
		Model:             "mock",
		ExecutedRealModel: false,
		StartedAt:         startedAt,
	}

	if err := validateScenario(scenario); err != nil {
		return finishFailedMockResult(result, err)
	}

	registry, validator, err := mockRegistryAndValidator(scenario)
	if err != nil {
		return finishFailedMockResult(result, err)
	}
	traceStore := trace.NewMemoryStoreWithEntries(nil)
	runtime := agent.NewRuntime(agent.RuntimeConfig{
		MaxSteps:    scenario.MaxSteps,
		LLMTimeout:  time.Second,
		ToolTimeout: time.Second,
		Model:       "mock",
	}, llm.NewMockProvider(cloneScenario(scenario).Actions), registry, validator, traceStore)

	diagnosis, runErr := runtime.Run(ctx, scenario.Goal)
	entries := traceStore.List()
	result.TraceSteps = len(entries)
	result.Assertions = EvaluateScenario(scenario, diagnosis, entries)
	if runErr != nil {
		return finishFailedMockResult(result, runErr)
	}
	if AllPassed(result.Assertions) {
		result.Status = StatusPassed
	} else {
		result.Status = StatusFailed
	}
	return finishResult(result), nil
}

// EvaluateScenario applies a scenario's portable expectations to a diagnosis
// and trace. It is intentionally independent of RunMock so an explicit
// real-model evaluator can use exactly the same acceptance criteria.
func EvaluateScenario(scenario Scenario, diagnosis *schema.Diagnosis, entries []trace.Entry) []Assertion {
	assertions := make([]Assertion, 0, 2+len(scenario.Expectations.ToolNames)+len(scenario.Expectations.SummaryContains))
	assertions = append(assertions, Assertion{
		Name:   "final_diagnosis",
		Passed: diagnosis != nil,
		Detail: assertionDetail(diagnosis != nil, "no final diagnosis was produced"),
	})

	seenTools := make(map[string]struct{}, len(entries))
	observedTools := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name := strings.TrimSpace(entry.ToolName); name != "" {
			seenTools[name] = struct{}{}
			observedTools = append(observedTools, name)
		}
	}
	for _, name := range scenario.Expectations.ToolNames {
		_, present := seenTools[name]
		assertions = append(assertions, Assertion{
			Name:   "required_tool:" + name,
			Passed: present,
			Detail: assertionDetail(present, "required tool was not present in trace"),
		})
	}
	if len(scenario.Expectations.ToolSequence) > 0 {
		matched := containsToolSequence(observedTools, scenario.Expectations.ToolSequence)
		assertions = append(assertions, Assertion{
			Name:   "tool_sequence",
			Passed: matched,
			Detail: assertionDetail(matched, "trace did not contain the required tool sequence"),
		})
	}

	for index, expected := range scenario.Expectations.SummaryContains {
		present := diagnosis != nil && strings.Contains(diagnosis.Summary, expected)
		assertions = append(assertions, Assertion{
			Name:   fmt.Sprintf("summary_contains:%d", index+1),
			Passed: present,
			Detail: assertionDetail(present, "final summary did not include the required text"),
		})
	}

	return assertions
}

// containsToolSequence reports whether expected appears in observed while
// preserving the expected tool order and allowing unrelated trace entries.
func containsToolSequence(observed, expected []string) bool {
	if len(expected) == 0 {
		return true
	}
	nextExpected := 0
	for _, toolName := range observed {
		if toolName == expected[nextExpected] {
			nextExpected++
			if nextExpected == len(expected) {
				return true
			}
		}
	}
	return false
}

// AllPassed reports whether every assertion passed. It is useful to callers
// that persist a Result and decide their own process exit status.
func AllPassed(assertions []Assertion) bool {
	for _, assertion := range assertions {
		if !assertion.Passed {
			return false
		}
	}
	return true
}

// assertionDetail omits detail for a passing assertion and keeps only the
// concise failure reason for a failed assertion.
func assertionDetail(passed bool, failure string) string {
	if passed {
		return ""
	}
	return failure
}

// finishFailedMockResult finalizes a redacted runner failure so it remains
// safe to persist alongside successfully executed scenarios.
func finishFailedMockResult(result Result, runErr error) (Result, error) {
	result.Status = StatusFailed
	result.Error = tools.RedactSensitive(runErr.Error())
	result = finishResult(result)
	return result, fmt.Errorf("run mock scenario %q: %s", result.ScenarioID, result.Error)
}

// finishResult stamps a result with a non-negative elapsed duration.
func finishResult(result Result) Result {
	finishedAt := time.Now().UTC()
	result.FinishedAt = finishedAt
	result.DurationMS = finishedAt.Sub(result.StartedAt).Milliseconds()
	if result.DurationMS < 0 {
		result.DurationMS = 0
	}
	return result
}

// validateScenario checks the invariant required to run a deterministic
// scenario without resolving real tools or targets.
func validateScenario(scenario Scenario) error {
	if strings.TrimSpace(scenario.ID) == "" {
		return errors.New("scenario id is required")
	}
	if strings.TrimSpace(scenario.Name) == "" {
		return errors.New("scenario name is required")
	}
	if strings.TrimSpace(scenario.Goal) == "" {
		return errors.New("scenario goal is required")
	}
	if scenario.MaxSteps <= 0 {
		return errors.New("scenario max steps must be positive")
	}
	if len(scenario.Actions) == 0 {
		return errors.New("scenario requires at least one action")
	}

	fixtures := make(map[string]struct{}, len(scenario.Tools))
	for _, fixture := range scenario.Tools {
		name := strings.TrimSpace(fixture.Name)
		if name == "" {
			return errors.New("tool fixture name is required")
		}
		if _, exists := fixtures[name]; exists {
			return fmt.Errorf("duplicate tool fixture %q", name)
		}
		fixtures[name] = struct{}{}
	}
	for _, action := range scenario.Actions {
		if action.Type != schema.ActionTypeToolCall {
			continue
		}
		if _, exists := fixtures[action.Tool]; !exists {
			return fmt.Errorf("tool action references missing fixture %q", action.Tool)
		}
	}
	return nil
}

// mockRegistryAndValidator constructs an isolated registry and allowlist from
// one scenario's in-memory tool fixtures.
func mockRegistryAndValidator(scenario Scenario) (*tools.Registry, *policy.Validator, error) {
	registry := tools.NewRegistry()
	allowlist := make([]string, 0, len(scenario.Tools))
	schemas := make(map[string]tools.ToolSchema, len(scenario.Tools))
	for _, fixture := range scenario.Tools {
		tool := fixtureTool{fixture: cloneToolFixture(fixture)}
		if err := registry.Register(&tool); err != nil {
			return nil, nil, err
		}
		allowlist = append(allowlist, fixture.Name)
		schemas[fixture.Name] = cloneToolSchema(fixture.Schema)
	}
	return registry, policy.NewValidator(policy.Config{
		ToolAllowlist: allowlist,
		ToolSchemas:   schemas,
	}), nil
}

type fixtureTool struct {
	fixture ToolFixture
}

// Spec describes the immutable mock fixture to the runtime.
func (t *fixtureTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        t.fixture.Name,
		Description: t.fixture.Description,
		Schema:      cloneToolSchema(t.fixture.Schema),
	}
}

// Run returns the fixture's copied observation or configured error without
// making a network, file-system, or subprocess call.
func (t *fixtureTool) Run(ctx context.Context, _ json.RawMessage) (schema.Observation, error) {
	if err := ctx.Err(); err != nil {
		return schema.Observation{Tool: t.fixture.Name}, err
	}
	observation := cloneObservation(t.fixture.Observation)
	if observation.Tool == "" {
		observation.Tool = t.fixture.Name
	}
	if t.fixture.Error != "" {
		return observation, errors.New(t.fixture.Error)
	}
	return observation, nil
}

// cloneToolFixture copies the mutable schema and observation owned by a mock
// fixture before it enters a runtime registry.
func cloneToolFixture(input ToolFixture) ToolFixture {
	output := input
	output.Schema = cloneToolSchema(input.Schema)
	output.Observation = cloneObservation(input.Observation)
	return output
}
