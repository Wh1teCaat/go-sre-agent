package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memory "github.com/y2/go-sre-agent/internal/memory"
	runstore "github.com/y2/go-sre-agent/internal/run"
	sessionstore "github.com/y2/go-sre-agent/internal/session"
)

// runDiagnosis 将普通文本或 /diagnose 参数作为新 run 的目标，并沿用当前交互会话。
func (c *interactiveCLI) runDiagnosis(goal string) bool {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return false
	}
	if _, err := c.previewHistory(goal, c.config.Service, c.config.Environment); err != nil {
		fmt.Fprintf(c.output, "历史记忆读取失败，未开始诊断：%v\n", err)
		return false
	}
	var result diagnoseResult
	var taskErr error
	terminate := c.runTask(func(ctx context.Context) {
		opts := c.config
		opts.Goal = goal
		opts.SessionID = c.currentSession
		opts.NewSession = c.newSession
		opts.Progress = c.progress.Report
		result, taskErr = c.deps.startDiagnosis(ctx, opts, c.options.MockScenario)
		taskErr = c.deps.saveDiagnosis(result, taskErr)
	})
	c.recordInteractiveResult(result)
	if taskErr != nil {
		c.printTaskFailure("诊断", result, taskErr)
		return terminate
	}
	if _, err := markdownOutput("", result); err != nil {
		fmt.Fprintf(c.output, "诊断完成，但报告保存失败：%v\n", err)
		return terminate
	}
	c.printDiagnosisCompletion(result)
	return terminate
}

// beginResume 在指定 run 时执行恢复；缺少 run ID 时只展示候选并等待用户选择。
func (c *interactiveCLI) beginResume(arguments []string) bool {
	if len(arguments) > 1 {
		fmt.Fprintln(c.output, "用法：/resume [run_id]")
		return false
	}
	if len(arguments) == 1 {
		return c.prepareResume(arguments[0])
	}
	candidates, err := c.resumableRuns()
	if err != nil {
		fmt.Fprintf(c.output, "读取可恢复运行失败：%v\n", err)
		return false
	}
	if len(candidates) == 0 {
		fmt.Fprintln(c.output, "没有可恢复的运行。")
		return false
	}
	pending := &interactivePending{kind: pendingResumeSelection, candidates: make(map[string]struct{}, len(candidates))}
	fmt.Fprintln(c.output, "可恢复运行：")
	for _, state := range candidates {
		pending.candidates[state.RunID] = struct{}{}
		fmt.Fprintf(c.output, "- %s  状态：%s  会话：%s\n", state.RunID, state.Status, displaySessionID(state.SessionID))
	}
	c.pending = pending
	fmt.Fprintln(c.output, "请输入要恢复的 run_id；直接回车取消。")
	return false
}

// prepareResume 保留 running run 的显式确认约束，其他状态直接进入恢复。
func (c *interactiveCLI) prepareResume(runID string) bool {
	state, err := c.runStore.Load(runID)
	if err != nil {
		fmt.Fprintf(c.output, "读取运行失败：%v\n", err)
		return false
	}
	if !resumableStatus(state.Status) || state.Diagnosis != nil {
		fmt.Fprintf(c.output, "运行 %s 当前状态为 %s，不能恢复。\n", state.RunID, state.Status)
		return false
	}
	if state.Status == runstore.StatusRunning {
		c.pending = &interactivePending{kind: pendingResumeRunning, runID: state.RunID}
		fmt.Fprintf(c.output, "运行 %s 仍标记为 running。请确认原进程已停止；输入 yes 才会继续恢复，直接回车取消。\n", state.RunID)
		return false
	}
	return c.runResume(state.RunID, false)
}

// runResume 调用已有恢复逻辑，自动加载与保存 session 和跨会话 memory。
func (c *interactiveCLI) runResume(runID string, resumeRunning bool) bool {
	previous, err := c.runStore.Load(runID)
	if err != nil {
		fmt.Fprintf(c.output, "读取运行失败：%v\n", err)
		return false
	}
	service := previous.Service
	if strings.TrimSpace(service) == "" {
		service = c.config.Service
	}
	environment := previous.Environment
	if strings.TrimSpace(environment) == "" {
		environment = c.config.Environment
	}
	if _, err := c.previewHistory(previous.Goal, service, environment); err != nil {
		fmt.Fprintf(c.output, "历史记忆读取失败，未开始恢复：%v\n", err)
		return false
	}
	var result diagnoseResult
	var taskErr error
	terminate := c.runTask(func(ctx context.Context) {
		result, taskErr = c.deps.resumeDiagnosis(ctx, resumeOptions{
			RunID:                  runID,
			RunDir:                 c.config.RunDir,
			SessionDir:             c.config.SessionDir,
			MemoryDir:              c.config.MemoryDir,
			Environment:            c.config.Environment,
			OverwriteSessionMemory: c.config.OverwriteSessionMemory,
			ConfigPath:             c.config.ConfigPath,
			Progress:               c.progress.Report,
			ResumeRunning:          resumeRunning,
		}, c.options.MockScenario)
		taskErr = c.deps.saveDiagnosis(result, taskErr)
	})
	c.recordInteractiveResult(result)
	if taskErr != nil {
		c.printTaskFailure("恢复", result, taskErr)
		return terminate
	}
	if _, err := markdownOutput("", result); err != nil {
		fmt.Fprintf(c.output, "恢复完成，但报告保存失败：%v\n", err)
		return terminate
	}
	c.printDiagnosisCompletion(result)
	return terminate
}

// resumableRuns 返回当前持久化目录中可由现有恢复规则继续的 run。
func (c *interactiveCLI) resumableRuns() ([]runstore.State, error) {
	runs, err := c.runStore.List()
	if err != nil {
		return nil, err
	}
	candidates := make([]runstore.State, 0, len(runs))
	for _, state := range runs {
		if resumableStatus(state.Status) && state.Diagnosis == nil {
			candidates = append(candidates, state)
		}
	}
	return candidates, nil
}

// recordInteractiveResult 更新交互状态，但只在底层确实生成 run ID 时更新最近运行引用。
func (c *interactiveCLI) recordInteractiveResult(result diagnoseResult) {
	if strings.TrimSpace(result.State.RunID) != "" {
		c.latestRun = result.State.RunID
	}
	if strings.TrimSpace(result.State.SessionID) == "" {
		return
	}
	c.currentSession = result.State.SessionID
	if _, err := c.sessionStore.Load(result.State.SessionID); err == nil {
		c.newSession = false
	}
}

// printTaskFailure 区分运行状态保存和诊断结果，避免把失败、取消或超时描述为成功完成。
func (c *interactiveCLI) printTaskFailure(operation string, result diagnoseResult, err error) {
	if result.State.RunID != "" {
		fmt.Fprintf(c.output, "%s运行结束：%s，状态：%s。\n", operation, result.State.RunID, result.State.Status)
	}
	fmt.Fprintf(c.output, "%s未成功完成：%v\n", operation, err)
}

// printDiagnosisCompletion 输出本次诊断的实际摘要和 run 引用，不将诊断完成表述为服务恢复。
func (c *interactiveCLI) printDiagnosisCompletion(result diagnoseResult) {
	if result.State.Diagnosis != nil {
		fmt.Fprintf(c.output, "诊断：%s\n", result.State.Diagnosis.Summary)
		if result.State.Diagnosis.RootCause != nil {
			fmt.Fprintf(c.output, "结论强度：%s\n", result.State.Diagnosis.RootCause.Status)
		}
	} else {
		fmt.Fprintln(c.output, "诊断运行完成，但没有生成最终诊断。")
	}
	fmt.Fprintf(c.output, "运行：%s\n", result.State.RunID)
	if strings.TrimSpace(result.ReportDir) != "" {
		fmt.Fprintln(c.output, "报告已保存，输入 /report 查看。")
	} else {
		fmt.Fprintln(c.output, "诊断运行完成，输入 /report 查看报告。")
	}
}

// previewHistory 按实际 runtime 相同的范围和预算查询跨会话 memory，并显示真实命中来源。
func (c *interactiveCLI) previewHistory(goal, service, environment string) ([]memory.Match, error) {
	matches, err := c.memoryStore.Search(memory.Query{
		Service:     service,
		Environment: environment,
		Goal:        goal,
		MaxMatches:  3,
		MaxBytes:    12 * 1024,
	})
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		fmt.Fprintln(c.output, "历史记忆：没有匹配记录，继续使用当前检查取证。")
		return matches, nil
	}
	fmt.Fprintf(c.output, "历史记忆：找到 %d 条相关记录，已作为排查参考。\n", len(matches))
	for index, match := range matches {
		fmt.Fprintf(c.output, "  [%d] 来源：%s，结论强度：%s，结果：%s\n", index+1, match.RunID, match.ConclusionStatus, match.Outcome)
	}
	return matches, nil
}

// showStatus 输出显式或最近 run 的现有机器可读状态，不通过模型生成解释。
func (c *interactiveCLI) showStatus(arguments []string) {
	state, ok := c.selectRun(arguments, "/status")
	if !ok {
		return
	}
	content, err := readDiagnosisStatus(runOptions{RunID: state.RunID, RunDir: c.config.RunDir})
	if err != nil {
		fmt.Fprintf(c.output, "读取状态失败：%v\n", err)
		return
	}
	fmt.Fprintln(c.output, content)
}

// showReport 渲染显式或最近 run 的报告，报告证据仍只来自该 run 的 trace。
func (c *interactiveCLI) showReport(arguments []string) {
	state, ok := c.selectRun(arguments, "/report")
	if !ok {
		return
	}
	content, err := renderDiagnosisReport(runOptions{RunID: state.RunID, RunDir: c.config.RunDir})
	if err != nil {
		fmt.Fprintf(c.output, "读取报告失败：%v\n", err)
		return
	}
	fmt.Fprintln(c.output, content)
}

// showPlan 展示持久化计划，而不是把模型内部推理暴露到终端。
func (c *interactiveCLI) showPlan(arguments []string) {
	state, ok := c.selectRun(arguments, "/plan")
	if !ok {
		return
	}
	c.printJSON(state.Plan)
}

// showEvidence 展示最终诊断引用的结构化证据和待验证项，保留当前 run 的证据边界。
func (c *interactiveCLI) showEvidence(arguments []string) {
	state, ok := c.selectRun(arguments, "/evidence")
	if !ok {
		return
	}
	if state.Diagnosis == nil {
		fmt.Fprintln(c.output, "该运行没有最终诊断或可展示的最终证据。")
		return
	}
	c.printJSON(struct {
		RunID                string `json:"run_id"`
		Evidence             any    `json:"evidence"`
		SupportingEvidence   any    `json:"supporting_evidence,omitempty"`
		CounterEvidence      any    `json:"counter_evidence,omitempty"`
		PendingVerifications any    `json:"pending_verifications,omitempty"`
	}{
		RunID:                state.RunID,
		Evidence:             state.Diagnosis.Evidence,
		SupportingEvidence:   state.Diagnosis.SupportingEvidence,
		CounterEvidence:      state.Diagnosis.CounterEvidence,
		PendingVerifications: state.Diagnosis.PendingVerifications,
	})
}

// selectRun 解析可选 run ID；缺省时只选择已维护的最近运行，不猜测其他运行。
func (c *interactiveCLI) selectRun(arguments []string, command string) (runstore.State, bool) {
	if len(arguments) > 1 {
		fmt.Fprintf(c.output, "用法：%s [run_id]\n", command)
		return runstore.State{}, false
	}
	runID := c.latestRun
	if len(arguments) == 1 {
		runID = arguments[0]
	}
	if strings.TrimSpace(runID) == "" {
		fmt.Fprintf(c.output, "%s 需要 run_id；当前会话还没有最近运行。\n", command)
		return runstore.State{}, false
	}
	state, err := c.runStore.Load(runID)
	if err != nil {
		fmt.Fprintf(c.output, "读取运行 %s 失败：%v\n", runID, err)
		return runstore.State{}, false
	}
	return state, true
}

// showRuns 列出当前会话明确关联的 run；未落盘的新会话不会伪造运行记录。
func (c *interactiveCLI) showRuns() {
	if c.newSession {
		fmt.Fprintln(c.output, "当前是尚未持久化的新会话，首次诊断后才会产生 run。")
		return
	}
	state, err := c.sessionStore.Load(c.currentSession)
	if err != nil {
		fmt.Fprintf(c.output, "读取当前会话失败：%v\n", err)
		return
	}
	if len(state.RunIDs) == 0 {
		fmt.Fprintln(c.output, "当前会话没有运行记录。")
		return
	}
	fmt.Fprintf(c.output, "会话 %s 的运行：\n", state.SessionID)
	for _, runID := range state.RunIDs {
		run, err := c.runStore.Load(runID)
		if err != nil {
			fmt.Fprintf(c.output, "- %s：读取失败（%v）\n", runID, err)
			continue
		}
		fmt.Fprintf(c.output, "- %s  状态：%s  目标：%s\n", run.RunID, run.Status, run.Goal)
	}
}

// showSessions 列出可用会话和其环境，供 /use 的显式切换操作使用。
func (c *interactiveCLI) showSessions() {
	sessions, err := c.sessionStore.List()
	if err != nil {
		fmt.Fprintf(c.output, "读取会话列表失败：%v\n", err)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintln(c.output, "没有已持久化的会话。")
		return
	}
	for _, state := range sessions {
		marker := " "
		if state.SessionID == c.currentSession {
			marker = "*"
		}
		fmt.Fprintf(c.output, "%s %s  环境：%s  运行数：%d\n", marker, state.SessionID, state.Environment, len(state.RunIDs))
	}
}

// useSession 仅在主循环空闲时调用，并复用 session 环境隔离校验。
func (c *interactiveCLI) useSession(arguments []string) {
	if len(arguments) != 1 {
		fmt.Fprintln(c.output, "用法：/use <session_id>")
		return
	}
	state, err := c.sessionStore.Load(arguments[0])
	if err != nil {
		fmt.Fprintf(c.output, "切换会话失败：%v\n", err)
		return
	}
	if state.Environment != c.config.Environment {
		fmt.Fprintf(c.output, "会话 %s 属于环境 %s，当前环境是 %s，不能切换。\n", state.SessionID, state.Environment, c.config.Environment)
		return
	}
	c.currentSession = state.SessionID
	c.newSession = false
	c.latestRun = ""
	if len(state.RunIDs) > 0 {
		c.latestRun = state.RunIDs[len(state.RunIDs)-1]
	}
	fmt.Fprintf(c.output, "已切换到会话：%s\n", c.currentSession)
}

// beginNewSession 立即创建一个空会话元数据并清除最近 run 引用；历史 run 和跨会话
// memory 均保留且仍会自动检索。首次诊断会以其目标替换占位会话目标。
func (c *interactiveCLI) beginNewSession() {
	now := c.options.Now().UTC()
	sessionID := sessionstore.NewID(now)
	if err := c.sessionStore.Save(sessionstore.State{
		SessionID:   sessionID,
		Goal:        "交互式会话（尚未开始诊断）",
		Environment: c.config.Environment,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		fmt.Fprintf(c.output, "创建新会话失败：%v\n", err)
		return
	}
	c.currentSession = sessionID
	c.latestRun = ""
	c.newSession = false
	fmt.Fprintf(c.output, "已创建新会话：%s。跨会话记忆仍会自动检索。\n", c.currentSession)
}

// showConfig 只展示不会泄露目标地址、连接串或凭据的启动配置摘要。
func (c *interactiveCLI) showConfig() {
	configPath := c.config.ConfigPath
	if strings.TrimSpace(configPath) == "" {
		configPath = "configs/config.yaml"
	}
	fmt.Fprintf(c.output, "配置文件：%s\n环境：%s\n服务：%s\n模式：%s\n运行目录：%s\n会话目录：%s\n记忆目录：%s\n", configPath, c.config.Environment, c.config.Service, c.modelLabel, c.config.RunDir, c.config.SessionDir, c.config.MemoryDir)
}

// displaySessionID 为旧 run 缺失会话关联时保留准确标记，不伪造 session ID。
func displaySessionID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return "legacy 未记录"
	}
	return sessionID
}

// printJSON 将已有结构化状态编码到安全终端输出；编码失败不会终止交互循环。
func (c *interactiveCLI) printJSON(value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(c.output, "编码输出失败：%v\n", err)
		return
	}
	fmt.Fprintln(c.output, string(data))
}
