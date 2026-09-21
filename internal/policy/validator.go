package policy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

// ValidateAction 校验 final 和 tool_call，避免未定义动作进入 runtime。
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

func (v *Validator) ValidatePlan(plan schema.Plan) error {
	if len(plan.Items) == 0 {
		return fmt.Errorf("plan update requires at least one item")
	}
	seen := map[string]struct{}{}
	for _, item := range plan.Items {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			return fmt.Errorf("plan item requires id")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate plan item id %q", id)
		}
		seen[id] = struct{}{}
		goal := strings.TrimSpace(item.Goal)
		if goal == "" {
			return fmt.Errorf("plan item %q requires goal", id)
		}
		if goal == id {
			return fmt.Errorf("plan item %q goal must describe the check instead of repeating its id", id)
		}
		if item.Status != "" && !validPlanStatus(item.Status) {
			return fmt.Errorf("plan item %q has unsupported status %q", id, item.Status)
		}
	}
	return nil
}

// ValidatePlanAdherence 要求有计划时的工具调用明确归属于其中一个检查项。
func (v *Validator) ValidatePlanAdherence(action schema.Action, plan *schema.Plan) error {
	if action.Type != schema.ActionTypeToolCall {
		return nil
	}
	id := strings.TrimSpace(action.PlanItemID)
	if plan == nil {
		if id != "" {
			return fmt.Errorf("tool action references plan item %q without an active plan", id)
		}
		return nil
	}
	if id == "" {
		return fmt.Errorf("tool action requires plan_item_id when plan is active")
	}
	for _, item := range plan.Items {
		if strings.TrimSpace(item.ID) == id {
			return nil
		}
	}
	return fmt.Errorf("tool action references unknown plan item %q", id)
}

func validPlanStatus(status string) bool {
	return status == "pending" || status == "done" || status == "blocked" || status == "insufficient"
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

	// 只用 step/tool 匹配，不比较模型写的 summary。
	// summary 最终会由报告层从真实 trace 中回填，避免模型改写证据内容。
	if err := validateEvidenceRefs(diagnosis.Evidence, entries); err != nil {
		return err
	}
	if err := validateEvidenceRefs(diagnosis.SupportingEvidence, entries); err != nil {
		return fmt.Errorf("supporting evidence: %w", err)
	}
	if err := validateEvidenceRefs(diagnosis.CounterEvidence, entries); err != nil {
		return fmt.Errorf("counter evidence: %w", err)
	}
	if err := validateEvidenceRoles(diagnosis); err != nil {
		return err
	}
	if err := validatePendingVerifications(diagnosis.PendingVerifications); err != nil {
		return err
	}

	traceEvidence := make(map[string]trace.Entry, len(entries))
	for _, entry := range entries {
		traceEvidence[schema.EvidenceKey(entry.Step, entry.ToolName)] = entry
	}
	for _, item := range diagnosis.Coverage {
		planItemID := strings.TrimSpace(item.PlanItemID)
		hasCompleted := false
		hasFailed := false
		for _, evidence := range item.Evidence {
			entry, ok := traceEvidence[schema.EvidenceKey(evidence.Step, evidence.Tool)]
			if !ok {
				return fmt.Errorf("coverage item %q references evidence step %d tool %q with no matching trace entry", planItemID, evidence.Step, evidence.Tool)
			}
			if executionStatus(entry) == schema.CheckExecutionCompleted {
				hasCompleted = true
			} else {
				hasFailed = true
			}
		}
		switch item.Status {
		case "done":
			if !hasCompleted {
				return fmt.Errorf("coverage item %q with status %q requires completed check evidence", planItemID, item.Status)
			}
		case "blocked":
			if !hasFailed {
				return fmt.Errorf("coverage item %q with status %q requires failed check evidence", planItemID, item.Status)
			}
		}
	}
	return validateRootCause(diagnosis, entries)
}

// validateEvidenceRoles 确保证据角色是互斥的，并且所有支持或反证都已列入
// final.evidence。兼容字段 Evidence 仍是最终报告的完整、可追溯证据目录。
func validateEvidenceRoles(diagnosis *schema.Diagnosis) error {
	all := make(map[string]struct{}, len(diagnosis.Evidence))
	for _, evidence := range diagnosis.Evidence {
		all[schema.EvidenceKey(evidence.Step, evidence.Tool)] = struct{}{}
	}
	supporting := make(map[string]struct{}, len(diagnosis.SupportingEvidence))
	for _, evidence := range diagnosis.SupportingEvidence {
		key := schema.EvidenceKey(evidence.Step, evidence.Tool)
		if _, exists := all[key]; !exists {
			return fmt.Errorf("supporting evidence step %d tool %q not present in final evidence", evidence.Step, evidence.Tool)
		}
		if _, exists := supporting[key]; exists {
			return fmt.Errorf("duplicate supporting evidence step %d tool %q", evidence.Step, evidence.Tool)
		}
		supporting[key] = struct{}{}
	}
	for _, evidence := range diagnosis.CounterEvidence {
		key := schema.EvidenceKey(evidence.Step, evidence.Tool)
		if _, exists := all[key]; !exists {
			return fmt.Errorf("counter evidence step %d tool %q not present in final evidence", evidence.Step, evidence.Tool)
		}
		if _, exists := supporting[key]; exists {
			return fmt.Errorf("evidence step %d tool %q cannot be both supporting and counter evidence", evidence.Step, evidence.Tool)
		}
	}
	return nil
}

// validatePendingVerifications 校验待验证项本身不含不完整的目标身份。
func validatePendingVerifications(items []schema.PendingVerification) error {
	for _, item := range items {
		if strings.TrimSpace(item.Question) == "" {
			return fmt.Errorf("pending verification requires question")
		}
		if (item.Target.Kind == "") != (item.Target.ID == "") {
			return fmt.Errorf("pending verification target requires both kind and id")
		}
	}
	return nil
}

// validateRootCause 校验最终诊断的结构化根因声明。结论强度由显式 status 表达
// 并绑定 trace 证据，policy 不解析自然语言措辞；措辞层面的语义约束由 skill 教给模型。
func validateRootCause(diagnosis *schema.Diagnosis, entries []trace.Entry) error {
	rootCause := diagnosis.RootCause
	if rootCause == nil {
		// 有证据的诊断必须显式声明根因状态，禁止只在 summary 里含混断言。
		if len(diagnosis.Evidence) > 0 {
			return fmt.Errorf(`final diagnosis requires root_cause; declare status "identified", "suspected", or "undetermined"`)
		}
		return nil
	}
	switch rootCause.Status {
	case "identified", "suspected":
		if strings.TrimSpace(rootCause.Statement) == "" {
			return fmt.Errorf("root cause status %q requires statement", rootCause.Status)
		}
		if rootCause.Status == "identified" && len(rootCause.Evidence) == 0 {
			return fmt.Errorf(`root cause status "identified" requires evidence; use "suspected" or "undetermined" without direct evidence`)
		}
	case "undetermined":
	default:
		return fmt.Errorf("unsupported root cause status %q", rootCause.Status)
	}
	if err := validateEvidenceRefs(rootCause.Evidence, entries); err != nil {
		return err
	}
	// 与 coverage 相同的约束：根因引用的证据必须同时出现在 final.evidence 中。
	finalEvidence := make(map[string]struct{}, len(diagnosis.Evidence))
	for _, evidence := range diagnosis.Evidence {
		finalEvidence[schema.EvidenceKey(evidence.Step, evidence.Tool)] = struct{}{}
	}
	for _, evidence := range rootCause.Evidence {
		if _, ok := finalEvidence[schema.EvidenceKey(evidence.Step, evidence.Tool)]; !ok {
			return fmt.Errorf("root cause references evidence step %d tool %q not present in final evidence", evidence.Step, evidence.Tool)
		}
	}

	supporting := evidenceKeySet(diagnosis.SupportingEvidence)
	if rootCause.Status == "identified" {
		if strings.TrimSpace(rootCause.FaultType) == "" {
			return fmt.Errorf(`root cause status "identified" requires fault_type`)
		}
		if len(diagnosis.SupportingEvidence) == 0 {
			return fmt.Errorf(`root cause status "identified" requires supporting_evidence`)
		}
		if len(diagnosis.CounterEvidence) > 0 {
			return fmt.Errorf(`root cause status "identified" cannot include counter_evidence; use "suspected" or "undetermined"`)
		}
		for _, evidence := range rootCause.Evidence {
			if _, ok := supporting[schema.EvidenceKey(evidence.Step, evidence.Tool)]; !ok {
				return fmt.Errorf("identified root cause evidence step %d tool %q must be supporting evidence", evidence.Step, evidence.Tool)
			}
		}
		return validateIdentifiedFaultType(rootCause.FaultType, rootCause.Evidence, entries)
	}
	if rootCause.Status == "suspected" {
		if len(diagnosis.SupportingEvidence) == 0 {
			return fmt.Errorf(`root cause status "suspected" requires supporting_evidence`)
		}
		if len(diagnosis.PendingVerifications) == 0 {
			return fmt.Errorf(`root cause status "suspected" requires pending_verifications`)
		}
		for _, evidence := range rootCause.Evidence {
			if _, ok := supporting[schema.EvidenceKey(evidence.Step, evidence.Tool)]; !ok {
				return fmt.Errorf("suspected root cause evidence step %d tool %q must be supporting evidence", evidence.Step, evidence.Tool)
			}
		}
	}
	return nil
}

// evidenceKeySet 将证据引用转换为便于交集判断的稳定键集合。
func evidenceKeySet(evidence []schema.Evidence) map[string]struct{} {
	keys := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		keys[schema.EvidenceKey(item.Step, item.Tool)] = struct{}{}
	}
	return keys
}

// validateIdentifiedFaultType 为常见故障定义最低必要证据，而不是尝试从 summary
// 的自然语言推导根因。规则是必要条件，不能替代当前运行中的人工或模型判断。
func validateIdentifiedFaultType(faultType string, evidence []schema.Evidence, entries []trace.Entry) error {
	byKey := make(map[string]trace.Entry, len(entries))
	for _, entry := range entries {
		byKey[schema.EvidenceKey(entry.Step, entry.ToolName)] = entry
	}
	selected := make([]trace.Entry, 0, len(evidence))
	for _, item := range evidence {
		if entry, ok := byKey[schema.EvidenceKey(item.Step, item.Tool)]; ok {
			selected = append(selected, entry)
		}
	}

	switch faultType {
	case "application_error_log":
		if hasCompletedTargetHealth(selected, []string{"http_check"}, schema.TargetHealthUnhealthy) &&
			hasCompletedTool(selected, "log_read") {
			return nil
		}
		return fmt.Errorf(`fault_type "application_error_log" requires completed unhealthy http_check evidence and completed log_read evidence`)
	case "dependency_unavailable":
		if hasCompletedTargetHealth(selected, []string{"postgres_ping", "postgres_check", "redis_ping", "redis_check", "kafka_check"}, schema.TargetHealthUnhealthy) {
			return nil
		}
		return fmt.Errorf(`fault_type "dependency_unavailable" requires completed direct dependency evidence with target_health "unhealthy"`)
	case "redis_instance_mismatch":
		for _, entry := range selected {
			if entry.ToolName == "redis_check" && executionStatus(entry) == schema.CheckExecutionCompleted && hasFalseFact(entry.Result, "instance_match") {
				return nil
			}
		}
		return fmt.Errorf(`fault_type "redis_instance_mismatch" requires completed redis_check evidence with fact "instance_match" = false`)
	case "websocket_handshake_rejected":
		if hasCompletedTargetHealth(selected, []string{"websocket_check"}, schema.TargetHealthUnhealthy) {
			return nil
		}
		return fmt.Errorf(`fault_type "websocket_handshake_rejected" requires completed websocket_check evidence with target_health "unhealthy"`)
	case "kafka_consumer_stall":
		for _, entry := range selected {
			if entry.ToolName != "kafka_check" || executionStatus(entry) != schema.CheckExecutionCompleted {
				continue
			}
			if entry.Result.TargetHealth == schema.TargetHealthDegraded || entry.Result.TargetHealth == schema.TargetHealthUnhealthy {
				if lag, ok := integerObservationValue(entry.Result, "active_lag"); ok && lag > 0 {
					return nil
				}
			}
		}
		return fmt.Errorf(`fault_type "kafka_consumer_stall" requires completed degraded kafka_check evidence with active_lag > 0`)
	default:
		return fmt.Errorf("unsupported identified fault_type %q", faultType)
	}
}

// hasCompletedTool 判断指定工具是否提供了已完成的检查结果。
func hasCompletedTool(entries []trace.Entry, toolName string) bool {
	for _, entry := range entries {
		if entry.ToolName == toolName && executionStatus(entry) == schema.CheckExecutionCompleted {
			return true
		}
	}
	return false
}

// hasCompletedTargetHealth 在指定工具中查找同时满足执行和目标健康状态的观测。
func hasCompletedTargetHealth(entries []trace.Entry, toolNames []string, health string) bool {
	allowed := make(map[string]struct{}, len(toolNames))
	for _, toolName := range toolNames {
		allowed[toolName] = struct{}{}
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.ToolName]; ok && executionStatus(entry) == schema.CheckExecutionCompleted && entry.Result.TargetHealth == health {
			return true
		}
	}
	return false
}

// hasFalseFact 判断 observation 的数据或事实中是否明确记录某键为 false。
func hasFalseFact(observation schema.Observation, key string) bool {
	value, ok := integerObservationValue(observation, key)
	if ok && value == 0 {
		return true
	}
	if observation.Data != nil {
		if value, ok := observation.Data[key].(bool); ok && !value {
			return true
		}
	}
	for _, fact := range observation.Facts {
		if fact.Key == key {
			if value, ok := fact.Value.(bool); ok && !value {
				return true
			}
		}
	}
	return false
}

// integerObservationValue 优先从原始数据、再从原子事实中读取整数值。
func integerObservationValue(observation schema.Observation, key string) (int, bool) {
	if observation.Data != nil {
		if value, ok := integerValue(observation.Data[key]); ok {
			return value, true
		}
	}
	for _, fact := range observation.Facts {
		if fact.Key == key {
			return integerValue(fact.Value)
		}
	}
	return 0, false
}

// integerValue 兼容 JSON 解码后常见的整数表示。
func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		value, err := typed.Int64()
		return int(value), err == nil
	case string:
		value, err := strconv.Atoi(typed)
		return value, err == nil
	default:
		return 0, false
	}
}

// executionStatus 兼容阶段三之前的 trace：旧记录没有 CheckStatus 时，仍按原有
// Error 语义判断检查是否完成；新记录优先使用显式状态。
func executionStatus(entry trace.Entry) string {
	if entry.Result.CheckStatus != "" {
		return entry.Result.CheckStatus
	}
	if entry.Error != "" {
		return schema.CheckExecutionFailed
	}
	return schema.CheckExecutionCompleted
}

// ValidateFinalCoverage 校验 final 是否覆盖当前 plan。
// 证据真实性和 plan item 归属统一交给 ValidateFinalEvidence。
func (v *Validator) ValidateFinalCoverage(diagnosis *schema.Diagnosis, plan schema.Plan) error {
	if diagnosis == nil || len(plan.Items) == 0 {
		return nil
	}
	planIDs := make(map[string]struct{}, len(plan.Items))
	for _, item := range plan.Items {
		planIDs[strings.TrimSpace(item.ID)] = struct{}{}
	}
	covered := make(map[string]schema.CoverageItem, len(diagnosis.Coverage))
	for _, item := range diagnosis.Coverage {
		id := strings.TrimSpace(item.PlanItemID)
		if id == "" {
			return fmt.Errorf("coverage item requires plan_item_id")
		}
		if !validCoverageStatus(item.Status) {
			return fmt.Errorf("coverage item %q has unsupported status %q", id, item.Status)
		}
		if _, ok := planIDs[id]; !ok {
			return fmt.Errorf("coverage item %q is not in current plan", id)
		}
		if _, ok := covered[id]; ok {
			return fmt.Errorf("duplicate coverage item %q", id)
		}
		covered[id] = item
	}
	for _, item := range plan.Items {
		id := strings.TrimSpace(item.ID)
		if _, ok := covered[id]; !ok {
			return fmt.Errorf("final coverage missing plan item %q", item.ID)
		}
	}

	// coverage 只引用 final.evidence；后者再由 ValidateFinalEvidence 统一回查 trace。
	// 因此这里能约束每个 coverage 项都有证据，又不会重复验证 trace。
	finalEvidence := make(map[string]struct{}, len(diagnosis.Evidence))
	for _, evidence := range diagnosis.Evidence {
		finalEvidence[schema.EvidenceKey(evidence.Step, evidence.Tool)] = struct{}{}
	}
	for _, item := range diagnosis.Coverage {
		if len(item.Evidence) == 0 {
			return fmt.Errorf("coverage item %q with status %q requires evidence", item.PlanItemID, item.Status)
		}
		for _, evidence := range item.Evidence {
			key := schema.EvidenceKey(evidence.Step, evidence.Tool)
			if _, ok := finalEvidence[key]; !ok {
				return fmt.Errorf("coverage item %q references evidence not present in final evidence", item.PlanItemID)
			}
		}
	}
	return nil
}

func validCoverageStatus(status string) bool {
	return status == "done" || status == "blocked" || status == "insufficient"
}

func validateEvidenceRefs(evidence []schema.Evidence, entries []trace.Entry) error {
	traceEvidence := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		traceEvidence[schema.EvidenceKey(entry.Step, entry.ToolName)] = struct{}{}
	}
	for _, item := range evidence {
		key := schema.EvidenceKey(item.Step, item.Tool)
		if _, ok := traceEvidence[key]; !ok {
			return fmt.Errorf("evidence step %d tool %q has no matching trace entry", item.Step, item.Tool)
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
