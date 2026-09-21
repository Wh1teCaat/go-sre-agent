// Package eval provides deterministic regression scenarios and result storage
// for the SRE agent. It intentionally has no dependency on command-line
// configuration or real LLM clients, so its mock runner is safe for tests and
// offline evaluation.
package eval

import (
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// Mode identifies the evaluator used to produce a Result.
type Mode string

const (
	// ModeMock is a deterministic run using the in-process mock provider and
	// fixture tools. It never contacts a model or a diagnostic target.
	ModeMock Mode = "mock"
	// ModeReal is reserved for an explicitly authorized real-model evaluation.
	// This package does not initiate such calls itself.
	ModeReal Mode = "real"
)

// Status is the outcome of an evaluation. Skipped is deliberately distinct
// from Passed so a non-executed real-model evaluation cannot be reported as a
// successful one.
type Status string

const (
	StatusPassed  Status = "passed"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
)

// Assertion records one deterministic expectation and its outcome.
type Assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// Expectations describe the checks shared by mock and real-model evaluators.
// ToolNames are required trace tool names, not an exhaustive trace allowlist.
type Expectations struct {
	ToolNames []string `json:"tool_names,omitempty"`
	// ToolSequence is an ordered subsequence expected in the trace. Extra
	// checks are permitted, which keeps an explicitly evaluated real model
	// from failing solely because it gathered additional valid evidence.
	ToolSequence    []string `json:"tool_sequence,omitempty"`
	SummaryContains []string `json:"summary_contains,omitempty"`
}

// ToolFixture is a deterministic in-memory implementation of one runtime
// tool. RunMock only returns Observation and Error from this value; it performs
// no network, file-system, or subprocess operation.
type ToolFixture struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Schema      tools.ToolSchema   `json:"schema"`
	Observation schema.Observation `json:"observation"`
	Error       string             `json:"error,omitempty"`
}

// Scenario is a fixed action sequence plus deterministic tool observations.
// It can be supplied to RunMock or used as expectations for a separately
// authorized real-model run.
type Scenario struct {
	ID           string          `json:"id"`
	Version      string          `json:"version,omitempty"`
	Name         string          `json:"name"`
	Goal         string          `json:"goal"`
	MaxSteps     int             `json:"max_steps"`
	Actions      []schema.Action `json:"actions"`
	Tools        []ToolFixture   `json:"tools,omitempty"`
	Expectations Expectations    `json:"expectations"`
}

// Result is a redactable, persistable record of one evaluation. RunID is
// optional because deterministic mock runs do not create a diagnostic run;
// callers running an explicitly authorized real evaluation may set it.
type Result struct {
	ID                string      `json:"id"`
	ScenarioID        string      `json:"scenario_id"`
	ScenarioName      string      `json:"scenario_name,omitempty"`
	ScenarioVersion   string      `json:"scenario_version,omitempty"`
	RunID             string      `json:"run_id,omitempty"`
	Command           string      `json:"command,omitempty"`
	Mode              Mode        `json:"mode"`
	Status            Status      `json:"status"`
	Model             string      `json:"model,omitempty"`
	ExecutedRealModel bool        `json:"executed_real_model"`
	StartedAt         time.Time   `json:"started_at"`
	FinishedAt        time.Time   `json:"finished_at"`
	DurationMS        int64       `json:"duration_ms"`
	TraceSteps        int         `json:"trace_steps,omitempty"`
	Assertions        []Assertion `json:"assertions,omitempty"`
	Error             string      `json:"error,omitempty"`
}
