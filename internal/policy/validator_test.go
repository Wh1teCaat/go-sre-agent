package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestValidatorAllowsWhitelistedTool(t *testing.T) {
	validator := NewValidator(Config{
		ToolAllowlist: []string{"http_check"},
	})

	action := schema.Action{
		Type: "tool_call",
		Tool: "http_check",
		Args: json.RawMessage(`{"url":"http://localhost:8080/health"}`),
	}

	if err := validator.ValidateAction(action); err != nil {
		t.Fatalf("validate action: %v", err)
	}
}

func TestValidatorRejectsDisallowedTool(t *testing.T) {
	validator := NewValidator(Config{
		ToolAllowlist: []string{"http_check"},
	})

	action := schema.Action{
		Type: "tool_call",
		Tool: "docker_exec",
		Args: json.RawMessage(`{}`),
	}

	if err := validator.ValidateAction(action); err == nil {
		t.Fatal("expected disallowed tool to fail validation")
	}
}

func TestValidatorAllowsFinalAction(t *testing.T) {
	validator := NewValidator(Config{
		ToolAllowlist: []string{"http_check"},
	})

	action := schema.Action{
		Type: "final",
		Final: &schema.Diagnosis{
			Summary: "service is healthy",
		},
	}

	if err := validator.ValidateAction(action); err != nil {
		t.Fatalf("validate final action: %v", err)
	}
}

func TestValidatorAllowsPlanUpdate(t *testing.T) {
	validator := NewValidator(Config{})
	plan := schema.Plan{
		Items: []schema.PlanItem{
			{ID: "backend", Goal: "检查后端服务是否存活", Status: "pending"},
			{ID: "logs", Goal: "检查后端日志", Status: "insufficient"},
		},
	}

	if err := validator.ValidatePlan(plan); err != nil {
		t.Fatalf("validate plan update: %v", err)
	}
}

func TestValidatorRejectsInvalidPlanUpdate(t *testing.T) {
	validator := NewValidator(Config{})
	plan := schema.Plan{
		Items: []schema.PlanItem{
			{ID: "", Goal: "检查后端服务是否存活"},
		},
	}

	err := validator.ValidatePlan(plan)
	if err == nil {
		t.Fatal("validate plan update succeeded, want item id error")
	}
	if !strings.Contains(err.Error(), "plan item requires id") {
		t.Fatalf("error = %q, want plan item id error", err.Error())
	}
}

func TestValidatorRejectsPlanGoalThatRepeatsID(t *testing.T) {
	err := NewValidator(Config{}).ValidatePlan(schema.Plan{Items: []schema.PlanItem{{
		ID:   "historical_request_analysis",
		Goal: "historical_request_analysis",
	}}})
	if err == nil || !strings.Contains(err.Error(), "repeating its id") {
		t.Fatalf("error = %v, want repeated goal error", err)
	}
}

func TestValidatorRejectsEmptyPlanUpdate(t *testing.T) {
	validator := NewValidator(Config{})
	err := validator.ValidatePlan(schema.Plan{})
	if err == nil || !strings.Contains(err.Error(), "plan update requires at least one item") {
		t.Fatalf("error = %v, want empty plan error", err)
	}
}

func TestValidatorRequiresToolCallToReferenceActivePlan(t *testing.T) {
	validator := NewValidator(Config{})
	plan := &schema.Plan{Items: []schema.PlanItem{{ID: "backend", Goal: "检查后端"}}}
	action := schema.Action{Type: schema.ActionTypeToolCall, Tool: "http_check"}

	if err := validator.ValidatePlanAdherence(action, plan); err == nil || !strings.Contains(err.Error(), "requires plan_item_id") {
		t.Fatalf("error = %v, want plan_item_id requirement", err)
	}
	action.PlanItemID = "backend"
	if err := validator.ValidatePlanAdherence(action, plan); err != nil {
		t.Fatalf("validate plan adherence: %v", err)
	}
}

func TestValidatorRejectsFinalActionWithoutSummary(t *testing.T) {
	validator := NewValidator(Config{})

	action := schema.Action{
		Type: schema.ActionTypeFinal,
		Final: &schema.Diagnosis{
			Summary: "   ",
		},
	}

	err := validator.ValidateAction(action)
	if err == nil {
		t.Fatal("validate final action succeeded, want summary error")
	}
	if !strings.Contains(err.Error(), "final diagnosis requires summary") {
		t.Fatalf("error = %q, want summary error", err.Error())
	}
}

func TestValidatorRequiresCoverageForEveryPlanItem(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary: "后端正常",
		Evidence: []schema.Evidence{
			{Step: 1, Tool: "http_check"},
		},
		Coverage: []schema.CoverageItem{
			{PlanItemID: "backend", Status: "done"},
		},
	}
	plan := schema.Plan{
		Items: []schema.PlanItem{
			{ID: "backend", Goal: "检查后端"},
			{ID: "redis", Goal: "检查 Redis"},
		},
	}

	err := validator.ValidateFinalCoverage(diagnosis, plan)
	if err == nil {
		t.Fatal("validate coverage succeeded, want missing plan item error")
	}
	if !strings.Contains(err.Error(), `final coverage missing plan item "redis"`) {
		t.Fatalf("error = %q, want missing redis coverage", err.Error())
	}
}

func TestValidatorRejectsCoverageWithoutFinalEvidenceReference(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary: "后端正常",
		Evidence: []schema.Evidence{
			{Step: 1, Tool: "http_check"},
		},
		Coverage: []schema.CoverageItem{
			{
				PlanItemID: "backend",
				Status:     "done",
				Evidence: []schema.Evidence{
					{Step: 2, Tool: "redis_ping"},
				},
			},
		},
	}
	plan := schema.Plan{Items: []schema.PlanItem{{ID: "backend", Goal: "检查后端"}}}

	err := validator.ValidateFinalCoverage(diagnosis, plan)
	if err == nil || !strings.Contains(err.Error(), "references evidence not present in final evidence") {
		t.Fatalf("error = %v, want coverage evidence reference error", err)
	}
}

func TestValidatorRejectsCompletedCoverageWithoutEvidence(t *testing.T) {
	validator := NewValidator(Config{})
	plan := schema.Plan{Items: []schema.PlanItem{{ID: "backend", Goal: "检查后端"}}}
	diagnosis := &schema.Diagnosis{
		Summary:  "后端正常",
		Coverage: []schema.CoverageItem{{PlanItemID: "backend", Status: "done"}},
	}

	err := validator.ValidateFinalCoverage(diagnosis, plan)
	if err == nil || !strings.Contains(err.Error(), "requires evidence") {
		t.Fatalf("error = %v, want completed coverage evidence error", err)
	}
}

func TestValidatorRejectsInsufficientCoverageWithoutAttemptEvidence(t *testing.T) {
	validator := NewValidator(Config{})
	plan := schema.Plan{Items: []schema.PlanItem{{ID: "logs", Goal: "检查日志"}}}
	diagnosis := &schema.Diagnosis{
		Summary:  "未检查日志",
		Coverage: []schema.CoverageItem{{PlanItemID: "logs", Status: "insufficient"}},
	}

	err := validator.ValidateFinalCoverage(diagnosis, plan)
	if err == nil || !strings.Contains(err.Error(), "requires evidence") {
		t.Fatalf("error = %v, want insufficient coverage evidence error", err)
	}
}

func TestValidatorRejectsDuplicateOrUnknownCoverageItems(t *testing.T) {
	validator := NewValidator(Config{})
	plan := schema.Plan{Items: []schema.PlanItem{{ID: "backend", Goal: "检查后端"}}}

	for name, coverage := range map[string][]schema.CoverageItem{
		"duplicate": {
			{PlanItemID: "backend", Status: "insufficient"},
			{PlanItemID: "backend", Status: "insufficient"},
		},
		"unknown": {
			{PlanItemID: "redis", Status: "insufficient"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validator.ValidateFinalCoverage(&schema.Diagnosis{Summary: "证据不足", Coverage: coverage}, plan)
			if err == nil {
				t.Fatal("validate coverage succeeded, want error")
			}
		})
	}
}

func TestValidatorAllowsCoveredPlanItems(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary:   "后端正常",
		RootCause: &schema.RootCause{Status: "undetermined"},
		Evidence: []schema.Evidence{
			{Step: 1, Tool: "http_check"},
		},
		Coverage: []schema.CoverageItem{
			{
				PlanItemID: "backend",
				Status:     "done",
				Evidence: []schema.Evidence{
					{Step: 1, Tool: "http_check"},
				},
			},
		},
	}
	plan := schema.Plan{
		Items: []schema.PlanItem{
			{ID: "backend", Goal: "检查后端"},
		},
	}
	if err := validator.ValidateFinalCoverage(diagnosis, plan); err != nil {
		t.Fatalf("validate coverage: %v", err)
	}
	entries := []trace.Entry{{Step: 1, PlanItemID: "backend", ToolName: "http_check"}}
	if err := validator.ValidateFinalEvidence(diagnosis, entries); err != nil {
		t.Fatalf("validate evidence: %v", err)
	}
}

func TestValidatorRequiresStructuredRootCause(t *testing.T) {
	validator := NewValidator(Config{})
	entries := []trace.Entry{
		{Step: 1, ToolName: "log_read"},
		{Step: 2, ToolName: "http_check"},
	}
	evidence := []schema.Evidence{{Step: 1, Tool: "log_read"}}

	for name, test := range map[string]struct {
		rootCause *schema.RootCause
		wantErr   string
	}{
		"missing root cause": {
			rootCause: nil,
			wantErr:   "requires root_cause",
		},
		"unsupported status": {
			rootCause: &schema.RootCause{Status: "confirmed", Statement: "密码错误"},
			wantErr:   `unsupported root cause status "confirmed"`,
		},
		"identified without evidence": {
			rootCause: &schema.RootCause{Status: "identified", Statement: "密码错误"},
			wantErr:   `root cause status "identified" requires evidence`,
		},
		"identified without statement": {
			rootCause: &schema.RootCause{Status: "identified", Evidence: evidence},
			wantErr:   "requires statement",
		},
		"suspected without statement": {
			rootCause: &schema.RootCause{Status: "suspected"},
			wantErr:   "requires statement",
		},
		"evidence outside trace": {
			rootCause: &schema.RootCause{Status: "identified", Statement: "密码错误", Evidence: []schema.Evidence{{Step: 99, Tool: "log_read"}}},
			wantErr:   "no matching trace entry",
		},
		"evidence outside final evidence": {
			rootCause: &schema.RootCause{Status: "identified", Statement: "密码错误", Evidence: []schema.Evidence{{Step: 2, Tool: "http_check"}}},
			wantErr:   "not present in final evidence",
		},
	} {
		t.Run(name, func(t *testing.T) {
			diagnosis := &schema.Diagnosis{Summary: "结论", Evidence: evidence, RootCause: test.rootCause}
			err := validator.ValidateFinalEvidence(diagnosis, entries)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}

	for name, rootCause := range map[string]*schema.RootCause{
		"undetermined": {Status: "undetermined"},
		"suspected":    {Status: "suspected", Statement: "疑似凭证问题，但证据不足"},
		"identified":   {Status: "identified", Statement: "日志证明请求命中密码校验失败分支", Evidence: evidence},
	} {
		t.Run("allows "+name, func(t *testing.T) {
			diagnosis := &schema.Diagnosis{Summary: "结论", Evidence: evidence, RootCause: rootCause}
			if err := validator.ValidateFinalEvidence(diagnosis, entries); err != nil {
				t.Fatalf("validate root cause: %v", err)
			}
		})
	}
}

func TestValidatorAllowsEvidenceToCoverComparisonPlanItem(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary:   "历史请求与复现现象相同，但根因不能确定。",
		RootCause: &schema.RootCause{Status: "undetermined"},
		Evidence: []schema.Evidence{
			{Step: 1, Tool: "log_read"},
			{Step: 2, Tool: "http_check"},
		},
		Coverage: []schema.CoverageItem{{
			PlanItemID: "compare_root_cause",
			Status:     "done",
			Evidence: []schema.Evidence{
				{Step: 1, Tool: "log_read"},
				{Step: 2, Tool: "http_check"},
			},
		}},
	}
	entries := []trace.Entry{
		{Step: 1, PlanItemID: "hist_request_trace", ToolName: "log_read"},
		{Step: 2, PlanItemID: "repro_valid_login", ToolName: "http_check"},
	}

	if err := validator.ValidateFinalEvidence(diagnosis, entries); err != nil {
		t.Fatalf("validate comparison evidence: %v", err)
	}
}

func TestValidatorRequiresCoverageStatusToMatchTraceOutcome(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		traceError string
		want       string
	}{
		{name: "done needs success", status: "done", traceError: "connection refused", want: "requires successful evidence"},
		{name: "blocked needs failure", status: "blocked", want: "requires failed evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator := NewValidator(Config{})
			diagnosis := &schema.Diagnosis{
				Summary:  "checked",
				Evidence: []schema.Evidence{{Step: 1, Tool: "http_check"}},
				Coverage: []schema.CoverageItem{{
					PlanItemID: "backend",
					Status:     test.status,
					Evidence:   []schema.Evidence{{Step: 1, Tool: "http_check"}},
				}},
			}
			entries := []trace.Entry{{Step: 1, PlanItemID: "backend", ToolName: "http_check", Error: test.traceError}}

			err := validator.ValidateFinalEvidence(diagnosis, entries)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidatorAllowsInsufficientCoverageAfterAttempt(t *testing.T) {
	validator := NewValidator(Config{})
	plan := schema.Plan{Items: []schema.PlanItem{{ID: "logs", Goal: "检查日志"}}}
	evidence := schema.Evidence{Step: 1, Tool: "log_read"}
	diagnosis := &schema.Diagnosis{
		Summary:   "日志证据不足",
		RootCause: &schema.RootCause{Status: "undetermined"},
		Evidence:  []schema.Evidence{evidence},
		Coverage:  []schema.CoverageItem{{PlanItemID: "logs", Status: "insufficient", Evidence: []schema.Evidence{evidence}}},
	}
	entries := []trace.Entry{{Step: 1, PlanItemID: "logs", ToolName: "log_read"}}

	if err := validator.ValidateFinalCoverage(diagnosis, plan); err != nil {
		t.Fatalf("validate coverage: %v", err)
	}
	if err := validator.ValidateFinalEvidence(diagnosis, entries); err != nil {
		t.Fatalf("validate evidence: %v", err)
	}
}

func TestValidatorRejectsMissingRequiredToolArg(t *testing.T) {
	validator := NewValidator(Config{
		ToolAllowlist: []string{"http_check"},
		ToolSchemas: map[string]tools.ToolSchema{
			"http_check": {
				Properties: map[string]tools.ArgSpec{
					"url": {Type: "string", Required: true},
				},
			},
		},
	})

	action := schema.Action{
		Type: schema.ActionTypeToolCall,
		Tool: "http_check",
		Args: json.RawMessage(`{}`),
	}

	err := validator.ValidateAction(action)
	if err == nil {
		t.Fatal("validate action succeeded, want missing required arg error")
	}
	if !strings.Contains(err.Error(), `missing required tool arg "url"`) {
		t.Fatalf("error = %q, want missing url", err.Error())
	}
}

func TestValidatorRejectsWrongToolArgType(t *testing.T) {
	validator := NewValidator(Config{
		ToolAllowlist: []string{"log_read"},
		ToolSchemas: map[string]tools.ToolSchema{
			"log_read": {
				Properties: map[string]tools.ArgSpec{
					"path":  {Type: "string", Required: true},
					"lines": {Type: "number"},
				},
			},
		},
	})

	action := schema.Action{
		Type: schema.ActionTypeToolCall,
		Tool: "log_read",
		Args: json.RawMessage(`{"path": 123}`),
	}

	err := validator.ValidateAction(action)
	if err == nil {
		t.Fatal("validate action succeeded, want type error")
	}
	if !strings.Contains(err.Error(), `tool arg "path" must be string`) {
		t.Fatalf("error = %q, want path type error", err.Error())
	}
}

func TestValidatorRejectsUnknownToolArgWhenSchemaIsKnown(t *testing.T) {
	validator := NewValidator(Config{
		ToolAllowlist: []string{"http_check"},
		ToolSchemas: map[string]tools.ToolSchema{
			"http_check": {
				Properties: map[string]tools.ArgSpec{
					"url": {Type: "string", Required: true},
				},
			},
		},
	})

	action := schema.Action{
		Type: schema.ActionTypeToolCall,
		Tool: "http_check",
		Args: json.RawMessage(`{"url":"http://localhost:8080/health","unexpected":true}`),
	}

	err := validator.ValidateAction(action)
	if err == nil {
		t.Fatal("validate action succeeded, want unknown arg error")
	}
	if !strings.Contains(err.Error(), `unknown tool arg "unexpected"`) {
		t.Fatalf("error = %q, want unknown arg error", err.Error())
	}
}

func TestValidatorRejectsFinalEvidenceWithoutTraceMatch(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary: "backend is alive",
		Evidence: []schema.Evidence{
			{Step: 1, Tool: "redis_ping", Summary: "model used the wrong tool"},
		},
	}
	entries := []trace.Entry{
		{Step: 1, ToolName: "http_check"},
	}

	err := validator.ValidateFinalEvidence(diagnosis, entries)
	if err == nil {
		t.Fatal("validate final evidence succeeded, want trace match error")
	}
	if !strings.Contains(err.Error(), `evidence step 1 tool "redis_ping" has no matching trace entry`) {
		t.Fatalf("error = %q, want trace match error", err.Error())
	}
}

func TestValidatorRejectsFinalEvidenceWithoutTraceStep(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary: "backend is alive",
		Evidence: []schema.Evidence{
			{Step: 99, Tool: "http_check", Summary: "model invented a step"},
		},
	}
	entries := []trace.Entry{
		{Step: 1, ToolName: "http_check"},
	}

	err := validator.ValidateFinalEvidence(diagnosis, entries)
	if err == nil {
		t.Fatal("validate final evidence succeeded, want trace step error")
	}
	if !strings.Contains(err.Error(), `evidence step 99 tool "http_check" has no matching trace entry`) {
		t.Fatalf("error = %q, want trace step error", err.Error())
	}
}

func TestValidatorAllowsTraceBackedFinalEvidence(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary:   "backend is alive",
		RootCause: &schema.RootCause{Status: "undetermined"},
		Evidence: []schema.Evidence{
			{Step: 1, Tool: "http_check", Summary: "backend returned 200"},
		},
	}
	entries := []trace.Entry{
		{Step: 1, ToolName: "http_check"},
	}

	if err := validator.ValidateFinalEvidence(diagnosis, entries); err != nil {
		t.Fatalf("validate final evidence: %v", err)
	}
}

func TestValidatorRequiresEvidenceWhenTraceExists(t *testing.T) {
	validator := NewValidator(Config{})
	diagnosis := &schema.Diagnosis{
		Summary: "backend is alive",
	}
	entries := []trace.Entry{
		{Step: 1, ToolName: "http_check"},
	}

	err := validator.ValidateFinalEvidence(diagnosis, entries)
	if err == nil {
		t.Fatal("validate final evidence succeeded, want missing evidence error")
	}
	if !strings.Contains(err.Error(), "final diagnosis requires evidence when trace exists") {
		t.Fatalf("error = %q, want missing evidence error", err.Error())
	}
}
