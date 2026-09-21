package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
	appconfig "github.com/y2/go-sre-agent/internal/config"
	evaluation "github.com/y2/go-sre-agent/internal/eval"
	"github.com/y2/go-sre-agent/internal/llm"
	"github.com/y2/go-sre-agent/internal/policy"
	runstore "github.com/y2/go-sre-agent/internal/run"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/trace"
)

const defaultEvaluationResultsDir = "evals/results"

type storedEvaluation struct {
	ResultPath string            `json:"result_path,omitempty"`
	Result     evaluation.Result `json:"result"`
}

type evaluationOutput struct {
	Mode    evaluation.Mode    `json:"mode"`
	Status  evaluation.Status  `json:"status"`
	Results []storedEvaluation `json:"results"`
}

type modelEvaluationOptions struct {
	Scenario         string
	ConfigPath       string
	ResultsDir       string
	ExecuteRealModel bool
}

// runEvalCommand dispatches the offline mock suite and the explicitly gated
// real-model evaluator. The latter is deliberately not reached by diagnose,
// normal tests, or a bare `eval model` invocation.
func runEvalCommand(args []string) {
	if len(args) == 0 {
		printUsageAndExit()
	}

	switch args[0] {
	case "mock":
		runMockEvaluationCommand(args[1:])
	case "model":
		runModelEvaluationCommand(args[1:])
	default:
		printUsageAndExit()
	}
}

// runMockEvaluationCommand parses mock-evaluation flags, writes its JSON
// result to stdout, and reports operational failures on stderr.
func runMockEvaluationCommand(args []string) {
	fs := flag.NewFlagSet("eval mock", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scenarioID := fs.String("scenario", "all", "fixed scenario id, or all")
	resultsDir := fs.String("results-dir", defaultEvaluationResultsDir, "evaluation result directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := rejectUnexpectedEvalArgs(fs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	results, err := runMockEvaluations(context.Background(), *scenarioID, *resultsDir)
	if len(results) > 0 {
		outputEvaluationResults(os.Stdout, evaluation.ModeMock, results)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runModelEvaluationCommand parses the explicit real-model gate before
// delegating evaluation and keeps the machine-readable result on stdout.
func runModelEvaluationCommand(args []string) {
	fs := flag.NewFlagSet("eval model", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scenarioID := fs.String("scenario", "login-500", "fixed scenario id")
	configPath := fs.String("config", "", "config file path used to select the evaluation skill")
	resultsDir := fs.String("results-dir", defaultEvaluationResultsDir, "evaluation result directory")
	executeRealModel := fs.Bool("execute-real-model", false, "authorize a real model request that may incur cost")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := rejectUnexpectedEvalArgs(fs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	result, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:         *scenarioID,
		ConfigPath:       *configPath,
		ResultsDir:       *resultsDir,
		ExecuteRealModel: *executeRealModel,
	})
	if result.Result.ID != "" {
		outputEvaluationResults(os.Stdout, evaluation.ModeReal, []storedEvaluation{result})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runMockEvaluations runs only in-process tool fixtures and an in-process mock
// provider. It intentionally does not load .env or diagnostic configuration.
func runMockEvaluations(ctx context.Context, scenarioID, resultsDir string) ([]storedEvaluation, error) {
	scenarios, err := selectedMockEvaluationScenarios(scenarioID)
	if err != nil {
		return nil, err
	}
	store := evaluation.NewStore(evaluationResultsDir(resultsDir))
	results := make([]storedEvaluation, 0, len(scenarios))
	failures := make([]string, 0)
	for _, scenario := range scenarios {
		result, runErr := evaluation.RunMock(ctx, scenario)
		if result.ID == "" {
			result = failedEvaluationResult(scenario, evaluation.ModeMock, "eval mock --scenario "+scenario.ID, runErr)
		}
		resultPath, saveErr := store.Save(result)
		if saveErr != nil {
			result.Status = evaluation.StatusFailed
			result.Error = "evaluation result could not be persisted"
			resultPath = ""
		}
		results = append(results, storedEvaluation{ResultPath: resultPath, Result: result})
		if runErr != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", scenario.ID, tools.RedactSensitive(runErr.Error())))
		}
		if result.Status != evaluation.StatusPassed {
			failures = append(failures, fmt.Sprintf("%s: status=%s", scenario.ID, result.Status))
		}
		if saveErr != nil {
			failures = append(failures, fmt.Sprintf("%s: save result: %s", scenario.ID, tools.RedactSensitive(saveErr.Error())))
		}
	}
	if len(failures) > 0 {
		return results, fmt.Errorf("mock evaluation failed: %s", strings.Join(uniqueStrings(failures), "; "))
	}
	return results, nil
}

// selectedMockEvaluationScenarios resolves "all" or one stable scenario ID
// into fresh scenario copies for a mock run.
func selectedMockEvaluationScenarios(scenarioID string) ([]evaluation.Scenario, error) {
	scenarioID = strings.TrimSpace(scenarioID)
	if scenarioID == "" || scenarioID == "all" {
		return evaluation.BuiltinScenarios(), nil
	}
	scenario, ok := evaluation.LookupScenario(scenarioID)
	if !ok {
		return nil, fmt.Errorf("unknown mock evaluation scenario %q", scenarioID)
	}
	return []evaluation.Scenario{scenario}, nil
}

// runModelEvaluation always writes a result when its scenario is valid. A bare
// invocation is recorded as skipped before configuration or model setup is
// touched, making the cost boundary independently testable.
func runModelEvaluation(ctx context.Context, opts modelEvaluationOptions) (storedEvaluation, error) {
	scenarioID := strings.TrimSpace(opts.Scenario)
	if scenarioID == "" {
		scenarioID = "login-500"
	}
	scenario, ok := evaluation.LookupScenario(scenarioID)
	if !ok {
		return storedEvaluation{}, fmt.Errorf("unknown model evaluation scenario %q", scenarioID)
	}

	startedAt := time.Now().UTC()
	command := "eval model --scenario " + scenario.ID
	if opts.ExecuteRealModel {
		command += " --execute-real-model"
	}
	result := evaluation.Result{
		ID:              evaluation.NewResultID(startedAt),
		ScenarioID:      scenario.ID,
		ScenarioName:    scenario.Name,
		ScenarioVersion: scenario.Version,
		Command:         command,
		Mode:            evaluation.ModeReal,
		StartedAt:       startedAt,
	}
	if !opts.ExecuteRealModel {
		result.Status = evaluation.StatusSkipped
		result.Error = "real model evaluation was not authorized; pass --execute-real-model to allow a potentially billable request"
		finishEvaluationResult(&result)
		return saveEvaluationResult(result, opts.ResultsDir)
	}
	if scenario.ID != "login-500" {
		return failAndSaveModelEvaluation(result, opts.ResultsDir, fmt.Errorf("real model evaluation currently supports only scenario %q", "login-500"))
	}

	llmConfig, err := loadLLMConfig()
	if err != nil {
		return failAndSaveModelEvaluation(result, opts.ResultsDir, err)
	}
	if llmConfig.Provider == "" || llmConfig.Provider == llm.DefaultLLMProvider {
		return failAndSaveModelEvaluation(result, opts.ResultsDir, fmt.Errorf("real model evaluation requires a configured non-mock provider"))
	}
	result.Model = llmConfig.Model

	diagnosisRun, executedRealModel, err := runRealModelLoginEvaluation(ctx, scenario, opts.ConfigPath)
	result.ExecutedRealModel = executedRealModel
	result.TraceSteps = len(diagnosisRun.State.Trace)
	if diagnosisRun.State.Diagnosis != nil {
		result.Assertions = evaluation.EvaluateScenario(scenario, diagnosisRun.State.Diagnosis, diagnosisRun.State.Trace)
	}
	if err != nil {
		return failAndSaveModelEvaluation(result, opts.ResultsDir, err)
	}
	if !evaluation.AllPassed(result.Assertions) {
		result.Status = evaluation.StatusFailed
		result.Error = "real model result did not satisfy the fixed scenario assertions"
		finishEvaluationResult(&result)
		stored, saveErr := saveEvaluationResult(result, opts.ResultsDir)
		if saveErr != nil {
			return stored, saveErr
		}
		return stored, fmt.Errorf("model evaluation failed: %s", result.Error)
	}
	result.Status = evaluation.StatusPassed
	finishEvaluationResult(&result)
	return saveEvaluationResult(result, opts.ResultsDir)
}

// runRealModelLoginEvaluation sends requests only to the configured model
// provider. Diagnostic tools are locked in-process fixtures, so a model cannot
// turn this evaluation into a scan of localhost or user-configured targets.
func runRealModelLoginEvaluation(ctx context.Context, scenario evaluation.Scenario, configPath string) (diagnoseResult, bool, error) {
	baseConfig, err := evaluationBaseConfig(configPath)
	if err != nil {
		return diagnoseResult{}, false, err
	}
	startedAt := time.Now().UTC()
	runID := runstore.NewRunID(startedAt)
	fixture := newRealModelLoginFixture()
	setup := diagnoseOptions{
		Goal:        scenario.Goal,
		MaxSteps:    scenario.MaxSteps,
		LLMTimeout:  baseConfig.Agent.LLMTimeout,
		ToolTimeout: baseConfig.Agent.ToolTimeout,
		SkillPath:   baseConfig.Agent.SkillPath,
	}
	provider, model, err := buildLLMProvider(setup, "")
	if err != nil {
		result, runErr := failedModelEvaluationRun(runID, setup.Goal, startedAt, schema.Plan{}, nil, err)
		return result, false, runErr
	}
	registry, validator, err := realModelEvaluationRegistry(fixture)
	if err != nil {
		result, runErr := failedModelEvaluationRun(runID, setup.Goal, startedAt, schema.Plan{}, nil, err)
		return result, false, runErr
	}
	trackedProvider := &modelCallTrackingProvider{Provider: provider}
	traceStore := trace.NewMemoryStoreWithEntries(nil)
	runtime := agent.NewRuntime(agent.RuntimeConfig{
		MaxSteps:      setup.MaxSteps,
		LLMTimeout:    setup.LLMTimeout,
		ToolTimeout:   setup.ToolTimeout,
		Model:         model,
		TargetContext: fixture.targetContext(),
	}, trackedProvider, registry, validator, traceStore)
	diagnosis, runErr := runtime.Run(ctx, setup.Goal)
	if runErr != nil {
		result, err := failedModelEvaluationRun(runID, setup.Goal, startedAt, runtime.Plan(), traceStore.List(), runErr)
		return result, trackedProvider.called, err
	}
	return diagnoseResult{State: runstore.State{
		RunID:     runID,
		Goal:      setup.Goal,
		Status:    runstore.StatusCompleted,
		Plan:      runtime.Plan(),
		Diagnosis: diagnosis,
		Trace:     traceStore.List(),
		CreatedAt: startedAt,
		UpdatedAt: time.Now().UTC(),
	}}, trackedProvider.called, nil
}

// modelCallTrackingProvider distinguishes setup failures from an authorized
// attempt to invoke a real provider. It does not retain prompts or responses.
type modelCallTrackingProvider struct {
	llm.Provider
	called bool
}

// Plan records that the wrapped real provider was invoked, then delegates the
// plan request without retaining its prompt or response.
func (p *modelCallTrackingProvider) Plan(ctx context.Context, request llm.Request) (*schema.Plan, error) {
	p.called = true
	return p.Provider.Plan(ctx, request)
}

// Next records that the wrapped real provider was invoked, then delegates the
// next-action request without retaining its prompt or response.
func (p *modelCallTrackingProvider) Next(ctx context.Context, request llm.Request) (llm.Decision, error) {
	p.called = true
	return p.Provider.Next(ctx, request)
}

// failedModelEvaluationRun creates an in-memory failed run state without
// persisting untrusted provider error details as an evaluation artifact.
func failedModelEvaluationRun(runID, goal string, startedAt time.Time, plan schema.Plan, entries []trace.Entry, runErr error) (diagnoseResult, error) {
	state := runstore.State{
		RunID:     runID,
		Goal:      goal,
		Status:    runstore.StatusFailed,
		Plan:      plan,
		Trace:     entries,
		Error:     tools.RedactSensitive(runErr.Error()),
		CreatedAt: startedAt,
		UpdatedAt: time.Now().UTC(),
	}
	return diagnoseResult{State: state}, runErr
}

type realModelLoginFixture struct {
	loginURL string
	logPath  string
}

// newRealModelLoginFixture returns the only diagnostic target and log path
// that an explicitly authorized model evaluation may reference.
func newRealModelLoginFixture() realModelLoginFixture {
	return realModelLoginFixture{
		loginURL: "https://login.fixture.invalid/v1/user/login",
		logPath:  "/fixture/logs/app.log",
	}
}

// targetContext gives the model fixed, non-routable fixture metadata instead
// of operator-configured diagnostic targets.
func (f realModelLoginFixture) targetContext() map[string]any {
	return map[string]any{
		"backend_base_url":  "https://login.fixture.invalid",
		"allowed_post_urls": []string{f.loginURL},
		"log_file":          f.logPath,
		"evaluation_notice": "All diagnostic tools are fixed in-process fixtures; do not request external targets.",
	}
}

// realModelEvaluationRegistry registers only locked in-process tools and the
// matching policy needed by the login-500 model evaluation.
func realModelEvaluationRegistry(fixture realModelLoginFixture) (*tools.Registry, *policy.Validator, error) {
	registry := tools.NewRegistry()
	fixtures := []evaluationFixtureTool{
		{
			name:        "http_check",
			description: "Fixed evaluation fixture. Only POST to the supplied fixture URL is accepted; no network request is made.",
			schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
				"url":    {Type: "string", Required: true},
				"method": {Type: "string", Required: true},
			}},
			validate: func(args map[string]any) error {
				if args["url"] != fixture.loginURL || strings.ToUpper(fmt.Sprint(args["method"])) != "POST" {
					return fmt.Errorf("http_check must use the fixed POST fixture URL")
				}
				return nil
			},
			observation: schema.Observation{
				Tool:    "http_check",
				Summary: "POST https://login.fixture.invalid/v1/user/login returned 500",
				Data:    map[string]any{"status": 500, "fixture": true},
			},
		},
		{
			name:        "log_read",
			description: "Fixed evaluation fixture. Only the supplied fixture log path is accepted; no file is read.",
			schema: tools.ToolSchema{Properties: map[string]tools.ArgSpec{
				"path":    {Type: "string", Required: true},
				"lines":   {Type: "number", Required: true},
				"keyword": {Type: "string", Required: true},
			}},
			validate: func(args map[string]any) error {
				if args["path"] != fixture.logPath || args["keyword"] != "ERROR" {
					return fmt.Errorf("log_read must use the fixed ERROR log fixture")
				}
				return nil
			},
			observation: schema.Observation{
				Tool:    "log_read",
				Summary: `read 1 log lines matching "ERROR"`,
				Data:    map[string]any{"lines": 1, "fixture": true},
			},
		},
	}
	schemas := make(map[string]tools.ToolSchema, len(fixtures))
	allowlist := make([]string, 0, len(fixtures))
	for index := range fixtures {
		fixtureTool := &fixtures[index]
		if err := registry.Register(fixtureTool); err != nil {
			return nil, nil, err
		}
		allowlist = append(allowlist, fixtureTool.name)
		schemas[fixtureTool.name] = fixtureTool.schema
	}
	return registry, policy.NewValidator(policy.Config{ToolAllowlist: allowlist, ToolSchemas: schemas}), nil
}

type evaluationFixtureTool struct {
	name        string
	description string
	schema      tools.ToolSchema
	validate    func(map[string]any) error
	observation schema.Observation
}

// Spec describes the locked fixture tool to the runtime and model provider.
func (t *evaluationFixtureTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: t.name, Description: t.description, Schema: t.schema}
}

// Run validates fixture arguments and returns a copied fixed observation; it
// never performs the network or file operation represented by the tool name.
func (t *evaluationFixtureTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	if err := ctx.Err(); err != nil {
		return schema.Observation{Tool: t.name}, err
	}
	args := map[string]any{}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{Tool: t.name}, fmt.Errorf("decode %s evaluation args: %w", t.name, err)
	}
	if err := t.validate(args); err != nil {
		return schema.Observation{Tool: t.name}, err
	}
	observation := t.observation
	if t.observation.Data != nil {
		observation.Data = make(map[string]any, len(t.observation.Data))
		for key, value := range t.observation.Data {
			observation.Data[key] = value
		}
	}
	return observation, nil
}

// evaluationBaseConfig loads only the agent settings needed by the model
// evaluator, falling back to safe built-in defaults when no path is set.
func evaluationBaseConfig(configPath string) (appconfig.Config, error) {
	if strings.TrimSpace(configPath) == "" {
		return appconfig.Default(), nil
	}
	return appconfig.Load(configPath)
}

// failAndSaveModelEvaluation records a sanitized failed result and returns a
// stable command error without exposing provider response details.
func failAndSaveModelEvaluation(result evaluation.Result, resultsDir string, err error) (storedEvaluation, error) {
	result.Status = evaluation.StatusFailed
	// Provider failures may include an untrusted response body. Do not persist
	// or echo it from this command; detailed provider diagnostics stay outside
	// evaluation records so an echoed credential cannot become an artifact.
	result.Error = "real model evaluation failed before completion; provider details were not persisted"
	finishEvaluationResult(&result)
	stored, saveErr := saveEvaluationResult(result, resultsDir)
	if saveErr != nil {
		return stored, saveErr
	}
	return stored, fmt.Errorf("model evaluation failed: %s", result.Error)
}

// failedEvaluationResult creates a persistable failure when a mock runner
// cannot return its own result record.
func failedEvaluationResult(scenario evaluation.Scenario, mode evaluation.Mode, command string, err error) evaluation.Result {
	startedAt := time.Now().UTC()
	result := evaluation.Result{
		ID:              evaluation.NewResultID(startedAt),
		ScenarioID:      scenario.ID,
		ScenarioName:    scenario.Name,
		ScenarioVersion: scenario.Version,
		Command:         command,
		Mode:            mode,
		Status:          evaluation.StatusFailed,
		StartedAt:       startedAt,
	}
	if err != nil {
		result.Error = tools.RedactSensitive(err.Error())
	}
	finishEvaluationResult(&result)
	return result
}

// saveEvaluationResult persists a result and changes its visible status to
// failed when persistence itself was unsuccessful.
func saveEvaluationResult(result evaluation.Result, resultsDir string) (storedEvaluation, error) {
	path, err := evaluation.NewStore(evaluationResultsDir(resultsDir)).Save(result)
	if err != nil {
		result.Status = evaluation.StatusFailed
		result.Error = "evaluation result could not be persisted"
		return storedEvaluation{Result: result}, err
	}
	return storedEvaluation{ResultPath: path, Result: result}, err
}

// finishEvaluationResult sets completion time and the non-negative duration
// for a command-owned evaluation result.
func finishEvaluationResult(result *evaluation.Result) {
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
}

// evaluationResultsDir returns the conventional ignored directory when the
// caller did not provide a result destination.
func evaluationResultsDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return defaultEvaluationResultsDir
	}
	return dir
}

// rejectUnexpectedEvalArgs prevents positional values from silently changing
// the meaning of boolean evaluation flags.
func rejectUnexpectedEvalArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
}

// outputEvaluationResults emits one JSON document and derives its overall
// status from the individual persisted or attempted results.
func outputEvaluationResults(writer *os.File, mode evaluation.Mode, results []storedEvaluation) {
	status := evaluation.StatusPassed
	for _, result := range results {
		if result.Result.Status == evaluation.StatusFailed {
			status = evaluation.StatusFailed
			break
		}
		if result.Result.Status == evaluation.StatusSkipped && status == evaluation.StatusPassed {
			status = evaluation.StatusSkipped
		}
	}
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(evaluationOutput{Mode: mode, Status: status, Results: results}); err != nil {
		fmt.Fprintln(os.Stderr, "encode evaluation output:", err)
	}
}

// uniqueStrings preserves first-seen failure messages while removing repeats.
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}
