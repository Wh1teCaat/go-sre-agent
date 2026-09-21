package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

// runEvalCommand 分派离线 mock 套件与需显式放行的真实模型评测。后者不会由
// diagnose、普通测试或未带授权参数的 `eval model` 调用触发。
func runEvalCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "mock":
		return runMockEvaluationCommand(args[1:], stdout, stderr)
	case "model":
		return runModelEvaluationCommand(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown eval command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

// runMockEvaluationCommand 解析 mock 评测参数，将 JSON 结果写入 stdout，并将
// 运行错误报告到 stderr。
func runMockEvaluationCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval mock", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scenarioID := fs.String("scenario", "all", "fixed scenario id, or all")
	resultsDir := fs.String("results-dir", defaultEvaluationResultsDir, "evaluation result directory")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := rejectUnexpectedEvalArgs(fs); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	results, err := runMockEvaluations(context.Background(), *scenarioID, *resultsDir)
	if len(results) > 0 {
		if outputErr := outputEvaluationResults(stdout, evaluation.ModeMock, results); outputErr != nil {
			fmt.Fprintln(stderr, outputErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// runModelEvaluationCommand 解析真实模型的显式放行参数后再委派评测，并将机器
// 可读结果保持在 stdout。
func runModelEvaluationCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval model", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scenarioID := fs.String("scenario", "login-500", "fixed scenario id")
	configPath := fs.String("config", "", "config file path used to select the evaluation skill")
	resultsDir := fs.String("results-dir", defaultEvaluationResultsDir, "evaluation result directory")
	executeRealModel := fs.Bool("execute-real-model", false, "authorize a real model request that may incur cost")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := rejectUnexpectedEvalArgs(fs); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	result, err := runModelEvaluation(context.Background(), modelEvaluationOptions{
		Scenario:         *scenarioID,
		ConfigPath:       *configPath,
		ResultsDir:       *resultsDir,
		ExecuteRealModel: *executeRealModel,
	})
	if result.Result.ID != "" {
		if outputErr := outputEvaluationResults(stdout, evaluation.ModeReal, []storedEvaluation{result}); outputErr != nil {
			fmt.Fprintln(stderr, outputErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// runMockEvaluations 只运行进程内工具样本和进程内 mock provider；它刻意不加载
// .env 或诊断配置。
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

// selectedMockEvaluationScenarios 将 "all" 或一个稳定场景 ID 解析为供 mock
// 运行使用的全新场景副本。
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

// runModelEvaluation 在场景有效时总会写入结果。未带授权参数的调用会在读取配置或
// 初始化模型前记录为 skipped，使成本边界可独立测试。
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

// runRealModelLoginEvaluation 只向已配置模型 provider 发送请求。诊断工具被锁定为
// 进程内样本，因此模型不能将该评测变成对 localhost 或用户配置目标的扫描。
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

// modelCallTrackingProvider 用于区分初始化失败与已授权的真实 provider 调用；
// 它不保留提示词或响应。
type modelCallTrackingProvider struct {
	llm.Provider
	called bool
}

// Plan 记录已调用被包装的真实 provider，再委派规划请求，不保留提示词或响应。
func (p *modelCallTrackingProvider) Plan(ctx context.Context, request llm.Request) (*schema.Plan, error) {
	p.called = true
	return p.Provider.Plan(ctx, request)
}

// Next 记录已调用被包装的真实 provider，再委派下一动作请求，不保留提示词或响应。
func (p *modelCallTrackingProvider) Next(ctx context.Context, request llm.Request) (llm.Decision, error) {
	p.called = true
	return p.Provider.Next(ctx, request)
}

// failedModelEvaluationRun 创建内存中的失败运行状态，不会将不可信 provider 错误
// 细节持久化为评测产物。
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

// newRealModelLoginFixture 返回显式授权模型评测唯一可引用的诊断目标和日志路径。
func newRealModelLoginFixture() realModelLoginFixture {
	return realModelLoginFixture{
		loginURL: "https://login.fixture.invalid/v1/user/login",
		logPath:  "/fixture/logs/app.log",
	}
}

// targetContext 向模型提供固定且不可路由的样本元数据，而不是操作者配置的诊断目标。
func (f realModelLoginFixture) targetContext() map[string]any {
	return map[string]any{
		"backend_base_url":  "https://login.fixture.invalid",
		"allowed_post_urls": []string{f.loginURL},
		"log_file":          f.logPath,
		"evaluation_notice": "All diagnostic tools are fixed in-process fixtures; do not request external targets.",
	}
}

// realModelEvaluationRegistry 只注册锁定的进程内工具，以及 login-500 模型评测
// 所需的匹配 policy。
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

// Spec 向 runtime 和模型 provider 描述锁定的样本工具。
func (t *evaluationFixtureTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: t.name, Description: t.description, Schema: t.schema}
}

// Run 校验样本参数并返回固定 observation 的副本；它绝不执行工具名所代表的网络或
// 文件操作。
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

// evaluationBaseConfig 只加载模型评测所需的 agent 设置；未指定路径时回退到安全的
// 内置默认值。
func evaluationBaseConfig(configPath string) (appconfig.Config, error) {
	if strings.TrimSpace(configPath) == "" {
		return appconfig.Default(), nil
	}
	return appconfig.Load(configPath)
}

// failAndSaveModelEvaluation 记录脱敏后的失败结果，并返回稳定的命令错误，不暴露
// provider 响应细节。
func failAndSaveModelEvaluation(result evaluation.Result, resultsDir string, err error) (storedEvaluation, error) {
	result.Status = evaluation.StatusFailed
	// Provider 失败可能包含不可信响应体。此命令不得持久化或回显它；详细 provider
	// 诊断信息应留在评测记录之外，避免回显的凭据成为产物。
	result.Error = "real model evaluation failed before completion; provider details were not persisted"
	finishEvaluationResult(&result)
	stored, saveErr := saveEvaluationResult(result, resultsDir)
	if saveErr != nil {
		return stored, saveErr
	}
	return stored, fmt.Errorf("model evaluation failed: %s", result.Error)
}

// failedEvaluationResult 在 mock runner 无法返回自身结果记录时创建可持久化失败结果。
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

// saveEvaluationResult 持久化结果；持久化自身失败时会将可见状态改为 failed。
func saveEvaluationResult(result evaluation.Result, resultsDir string) (storedEvaluation, error) {
	path, err := evaluation.NewStore(evaluationResultsDir(resultsDir)).Save(result)
	if err != nil {
		result.Status = evaluation.StatusFailed
		result.Error = "evaluation result could not be persisted"
		return storedEvaluation{Result: result}, err
	}
	return storedEvaluation{ResultPath: path, Result: result}, err
}

// finishEvaluationResult 为命令持有的评测结果设置完成时间和非负耗时。
func finishEvaluationResult(result *evaluation.Result) {
	result.FinishedAt = time.Now().UTC()
	result.DurationMS = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
}

// evaluationResultsDir 在调用方未提供结果目录时返回约定的、被 Git 忽略的目录。
func evaluationResultsDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return defaultEvaluationResultsDir
	}
	return dir
}

// rejectUnexpectedEvalArgs 防止位置参数静默改变布尔评测参数的含义。
func rejectUnexpectedEvalArgs(fs *flag.FlagSet) error {
	if fs.NArg() == 0 {
		return nil
	}
	return fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
}

// outputEvaluationResults 输出一个 JSON 文档，并从各已持久化或已尝试的结果推导
// 总体状态。
func outputEvaluationResults(writer io.Writer, mode evaluation.Mode, results []storedEvaluation) error {
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
		return fmt.Errorf("encode evaluation output: %w", err)
	}
	return nil
}

// uniqueStrings 保留首次出现的失败消息，同时移除重复项。
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
