package policy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

// ValidateAction 校验 LLM 输出的顶层 action 形态。
// 它只接受 final 和 tool_call，避免模型输出自然语言或未定义动作直接进入 runtime。
func (v *Validator) ValidateAction(action schema.Action) error {
	switch action.Type {
	case schema.ActionTypeFinal:
		if action.Final == nil {
			return fmt.Errorf("final action requires diagnosis")
		}
		if strings.TrimSpace(action.Final.Summary) == "" {
			return fmt.Errorf("final diagnosis requires summary")
		}
		return nil
	case schema.ActionTypeToolCall:
		return v.validateToolCall(action)
	default:
		return fmt.Errorf("unsupported action type %q", action.Type)
	}
}

// ValidateFinalEvidence 校验最终诊断引用的 evidence 是否真实来自本次 trace。
// 这是 policy 层的可信度约束：runtime 负责收集 trace，policy 负责判定模型输出能否被接受。
func (v *Validator) ValidateFinalEvidence(diagnosis *schema.Diagnosis, entries []trace.Entry) error {
	if diagnosis == nil {
		return nil
	}
	if len(entries) > 0 && len(diagnosis.Evidence) == 0 {
		return fmt.Errorf("final diagnosis requires evidence when trace exists")
	}

	traceEvidence := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		// 只用 step/tool 匹配，不比较模型写的 summary。
		// summary 最终会由报告层从真实 trace 中回填，避免模型改写证据内容。
		traceEvidence[schema.EvidenceKey(entry.Step, entry.ToolName)] = struct{}{}
	}
	for _, evidence := range diagnosis.Evidence {
		key := schema.EvidenceKey(evidence.Step, evidence.Tool)
		if _, ok := traceEvidence[key]; !ok {
			return fmt.Errorf("evidence step %d tool %q has no matching trace entry", evidence.Step, evidence.Tool)
		}
	}
	return nil
}

// validateToolCall 校验工具名、白名单和 JSON 参数对象。
// 具体工具是否存在由 registry 负责，这里先拦住策略层明确禁止的调用。
func (v *Validator) validateToolCall(action schema.Action) error {
	if action.Tool == "" {
		return fmt.Errorf("tool_call action requires tool name")
	}
	if len(v.allowed) > 0 {
		if _, ok := v.allowed[action.Tool]; !ok {
			return fmt.Errorf("tool %q is not allowed", action.Tool)
		}
	}
	if len(action.Args) == 0 {
		return fmt.Errorf("tool_call action requires args")
	}
	var decoded map[string]any
	if err := json.Unmarshal(action.Args, &decoded); err != nil {
		return fmt.Errorf("tool args must be valid JSON object: %w", err)
	}
	if decoded == nil {
		return fmt.Errorf("tool args must be a JSON object")
	}
	if schema, ok := v.schemas[action.Tool]; ok {
		// schema 来自 registry 中真实工具的声明，因此模型不能塞入工具不认识的参数。
		if err := validateArgsAgainstSchema(decoded, schema); err != nil {
			return err
		}
	}
	return nil
}

// validateArgsAgainstSchema 按工具声明的 schema 校验未知参数、必填参数和基础类型。
// 这里保持轻量，不做 URL、路径等业务语义校验，那些边界放在各工具内部处理。
func validateArgsAgainstSchema(args map[string]any, schema tools.ToolSchema) error {
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	argNames := make([]string, 0, len(args))
	for name := range args {
		argNames = append(argNames, name)
	}
	sort.Strings(argNames)
	for _, name := range argNames {
		if _, ok := schema.Properties[name]; !ok {
			return fmt.Errorf("unknown tool arg %q", name)
		}
	}

	for _, name := range names {
		spec := schema.Properties[name]
		value, exists := args[name]
		if spec.Required && !exists {
			return fmt.Errorf("missing required tool arg %q", name)
		}
		if !exists || spec.Type == "" {
			continue
		}
		// 这里校验的是 JSON 层面的粗类型。更细的约束，比如 URL scheme、
		// 日志路径 allowlist、主机 allowlist，留给各工具按业务语义处理。
		if !matchesArgType(value, spec.Type) {
			return fmt.Errorf("tool arg %q must be %s", name, spec.Type)
		}
	}
	return nil
}

// matchesArgType 做 JSON 解码后的基础类型匹配。
// Go 的 encoding/json 默认把数字解成 float64，所以 number 会兼容常见数字表示。
func matchesArgType(value any, argType string) bool {
	switch argType {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case float64, int, int64, json.Number:
			return true
		default:
			return false
		}
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	default:
		return true
	}
}
