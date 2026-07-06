package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

func TestValidatorAllowsWhitelistedTool(t *testing.T) {
	validator := NewValidator(Config{
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
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
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
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
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
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

func TestValidatorRejectsFinalActionWithoutSummary(t *testing.T) {
	validator := NewValidator(Config{
		MaxSteps:    3,
		ToolTimeout: time.Second,
	})

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

func TestValidatorRejectsMissingRequiredToolArg(t *testing.T) {
	validator := NewValidator(Config{
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
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
		MaxSteps:      3,
		ToolAllowlist: []string{"log_read"},
		ToolTimeout:   time.Second,
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
		MaxSteps:      3,
		ToolAllowlist: []string{"http_check"},
		ToolTimeout:   time.Second,
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
		Summary: "backend is alive",
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
