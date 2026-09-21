package eval

import (
	"encoding/json"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const (
	// ScenarioSkeleton validates the smallest final-diagnosis path.
	ScenarioSkeleton = "skeleton"
	// ScenarioLogin500 validates a fixed HTTP-plus-log evidence sequence.
	ScenarioLogin500       = "login-500"
	builtinScenarioVersion = "v1"
)

// BuiltinScenarios returns copies of the deterministic scenarios shipped with
// the binary. A caller may inspect or adapt the returned values without
// changing later evaluations.
func BuiltinScenarios() []Scenario {
	scenarios := []Scenario{
		skeletonScenario(),
		login500Scenario(),
	}
	for index := range scenarios {
		scenarios[index] = cloneScenario(scenarios[index])
	}
	return scenarios
}

// LookupScenario returns a copy of a built-in scenario by its stable ID.
func LookupScenario(id string) (Scenario, bool) {
	for _, scenario := range BuiltinScenarios() {
		if scenario.ID == id {
			return scenario, true
		}
	}
	return Scenario{}, false
}

// skeletonScenario builds the minimal final-diagnosis regression fixture.
func skeletonScenario() Scenario {
	return Scenario{
		ID:       ScenarioSkeleton,
		Version:  builtinScenarioVersion,
		Name:     "最小诊断闭环",
		Goal:     "验证固定 runtime 诊断闭环",
		MaxSteps: 1,
		Actions: []schema.Action{
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "固定 mock 场景直接生成最终诊断",
				Final: &schema.Diagnosis{
					Summary: "MVP 诊断闭环验证完成。",
				},
			},
		},
		Expectations: Expectations{
			SummaryContains: []string{"MVP 诊断闭环验证完成"},
		},
	}
}

// login500Scenario builds the fixed HTTP-and-log evidence regression fixture.
func login500Scenario() Scenario {
	return Scenario{
		ID:       ScenarioLogin500,
		Version:  builtinScenarioVersion,
		Name:     "登录接口 500",
		Goal:     "诊断登录接口为什么返回 500",
		MaxSteps: 3,
		Tools: []ToolFixture{
			{
				Name:        "http_check",
				Description: "固定 HTTP 500 observation",
				Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
					"url":    {Type: "string", Required: true},
					"method": {Type: "string", Required: true},
				}},
				Observation: schema.Observation{
					Tool:    "http_check",
					Summary: "returned HTTP 500",
					Data: map[string]any{
						"status": 500,
					},
				},
			},
			{
				Name:        "log_read",
				Description: "固定错误日志 observation",
				Schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
					"path":    {Type: "string", Required: true},
					"lines":   {Type: "number", Required: true},
					"keyword": {Type: "string", Required: true},
				}},
				Observation: schema.Observation{
					Tool:    "log_read",
					Summary: `read 1 log lines matching "ERROR"`,
					Data: map[string]any{
						"lines": 1,
					},
				},
			},
		},
		Actions: []schema.Action{
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "读取固定的登录 HTTP 响应",
				Tool:           "http_check",
				Args:           mustJSON(map[string]any{"url": "https://fixture.invalid/v1/user/login", "method": "POST"}),
			},
			{
				Type:           schema.ActionTypeToolCall,
				ThoughtSummary: "读取固定的登录错误日志",
				Tool:           "log_read",
				Args:           mustJSON(map[string]any{"path": "/fixture/app.log", "lines": 50, "keyword": "ERROR"}),
			},
			{
				Type:           schema.ActionTypeFinal,
				ThoughtSummary: "固定证据足以确认异常，但不足以定位根因",
				// root_cause stays in this JSON fixture so it satisfies the
				// currently checked-out validator. Older Diagnosis structs safely
				// ignore it, which keeps the phase-0 baseline independent of the
				// phase-3 Go field addition.
				Final: mustDiagnosis(`{
					"summary": "登录接口返回 500，已复现 HTTP 异常并读取相关错误日志。",
					"root_cause": {
						"status": "undetermined",
						"statement": "固定场景只证明接口异常与错误日志存在，不能定位具体根因。"
					},
					"evidence": [
						{"step": 1, "tool": "http_check", "summary": "登录接口返回 500"},
						{"step": 2, "tool": "log_read", "summary": "日志包含 ERROR"}
					],
					"recommendations": ["结合当前数据库、缓存和鉴权状态继续排查。"]
				}`),
			},
		},
		Expectations: Expectations{
			ToolNames:       []string{"http_check", "log_read"},
			ToolSequence:    []string{"http_check", "log_read"},
			SummaryContains: []string{"登录接口返回 500"},
		},
	}
}

// mustJSON encodes static fixture arguments and panics only for programmer
// errors in the hard-coded scenario definitions.
func mustJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

// mustDiagnosis decodes a static diagnosis fixture and panics only when that
// fixture is invalid at build time.
func mustDiagnosis(raw string) *schema.Diagnosis {
	var diagnosis schema.Diagnosis
	if err := json.Unmarshal([]byte(raw), &diagnosis); err != nil {
		panic(err)
	}
	return &diagnosis
}

// cloneScenario deep-copies mutable fixture data so callers cannot affect a
// later deterministic evaluation.
func cloneScenario(input Scenario) Scenario {
	output := input
	output.Actions = make([]schema.Action, len(input.Actions))
	for index, action := range input.Actions {
		output.Actions[index] = action
		output.Actions[index].Args = append(json.RawMessage(nil), action.Args...)
		output.Actions[index].Final = cloneDiagnosis(action.Final)
	}
	output.Tools = make([]ToolFixture, len(input.Tools))
	for index, fixture := range input.Tools {
		output.Tools[index] = fixture
		output.Tools[index].Schema = cloneToolSchema(fixture.Schema)
		output.Tools[index].Observation = cloneObservation(fixture.Observation)
	}
	output.Expectations.ToolNames = append([]string(nil), input.Expectations.ToolNames...)
	output.Expectations.ToolSequence = append([]string(nil), input.Expectations.ToolSequence...)
	output.Expectations.SummaryContains = append([]string(nil), input.Expectations.SummaryContains...)
	return output
}

// cloneToolSchema copies the property map used by a fixture tool.
func cloneToolSchema(input tools.ToolSchema) tools.ToolSchema {
	output := tools.ToolSchema{Properties: make(map[string]tools.ArgSpec, len(input.Properties))}
	for name, spec := range input.Properties {
		output.Properties[name] = spec
	}
	return output
}

// cloneObservation copies an observation and its mutable data map.
func cloneObservation(input schema.Observation) schema.Observation {
	output := input
	output.Data = cloneMap(input.Data)
	return output
}

// cloneDiagnosis round-trips a diagnosis to avoid coupling fixtures to fields
// introduced by later schema versions.
func cloneDiagnosis(input *schema.Diagnosis) *schema.Diagnosis {
	if input == nil {
		return nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return mustDiagnosis(string(data))
}

// cloneMap recursively copies map-backed observation data.
func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneValue(value)
	}
	return output
}

// cloneValue copies the collection shapes permitted in fixture observation
// data and leaves immutable scalar values unchanged.
func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		output := make([]any, len(typed))
		for index, item := range typed {
			output[index] = cloneValue(item)
		}
		return output
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}
