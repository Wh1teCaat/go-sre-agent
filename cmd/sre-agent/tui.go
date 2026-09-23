package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/y2/go-sre-agent/internal/agent"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
	"github.com/y2/go-sre-agent/internal/tui"
)

type tuiBackend struct{ cli *interactiveCLI }

func runTUI(options interactiveOptions) error {
	cli, err := newInteractiveCLI(options, interactiveDependencies{})
	if err != nil {
		return err
	}
	return tui.Run(&tuiBackend{cli: cli}, options.Input, options.Output)
}
func (b *tuiBackend) Header() tui.Header {
	return tui.Header{Environment: b.cli.config.Environment, Model: b.cli.modelLabel, SessionID: b.cli.currentSession}
}
func (b *tuiBackend) History() []tui.Message {
	c := b.cli
	if c.newSession {
		return nil
	}
	session, err := c.sessionStore.Load(c.currentSession)
	if err != nil {
		return []tui.Message{{Role: "system", Text: fmt.Sprintf("Could not load history: %v", err)}}
	}
	var messages []tui.Message
	for _, id := range session.RunIDs {
		state, err := c.runStore.Load(id)
		if err != nil {
			continue
		}
		messages = append(messages, tui.Message{Role: "user", Text: state.Goal})
		for _, entry := range state.Trace {
			if entry.ToolName != "" {
				s := entry.Result.Summary
				if entry.Result.Error != "" {
					s = entry.Result.Error
				}
				messages = append(messages, tui.Message{Role: "tool", Text: fmt.Sprintf("%s · %s", entry.ToolName, s)})
			}
		}
		if state.Diagnosis != nil {
			health, detail, issue, cause := tuiAssessment(diagnoseResult{State: state})
			messages = append(messages, tui.Message{Role: "assistant", Text: tui.CompletionText(tui.Completion{Summary: state.Diagnosis.Summary, Health: health, HealthDetail: detail, Issue: issue, Cause: cause})})
		} else {
			messages = append(messages, tui.Message{Role: "system", Text: fmt.Sprintf("Run %s: %s", id, state.Status)})
		}
	}
	if len(messages) > 120 {
		messages = messages[len(messages)-120:]
	}
	return messages
}
func (b *tuiBackend) Diagnose(ctx context.Context, goal string, emit func(agent.ProgressEvent)) tui.Completion {
	c := b.cli
	opts := c.config
	opts.Goal = goal
	opts.SessionID = c.currentSession
	opts.NewSession = c.newSession
	var loaded []schema.Memory
	memoryLoaded := false
	opts.Progress = func(e agent.ProgressEvent) {
		if e.Kind == agent.ProgressMemoryLoaded {
			memoryLoaded = true
			loaded = append([]schema.Memory(nil), e.Memories...)
		}
		emit(e)
	}
	result, runErr := c.deps.startDiagnosis(ctx, opts, c.options.MockScenario)
	collected, err := saveDiagnosisResultWithCollection(result, runErr)
	c.recordInteractiveResult(result)
	completion := tuiCompletion(result, c.currentSession, collected, err)
	completion.Memories = loaded
	completion.MemoryLoaded = memoryLoaded
	return completion
}

func (b *tuiBackend) Resume(ctx context.Context, runID string, resumeRunning bool, emit func(agent.ProgressEvent)) tui.Completion {
	c := b.cli
	var loaded []schema.Memory
	memoryLoaded := false
	progress := func(e agent.ProgressEvent) {
		if e.Kind == agent.ProgressMemoryLoaded {
			memoryLoaded = true
			loaded = append([]schema.Memory(nil), e.Memories...)
		}
		emit(e)
	}
	result, runErr := c.deps.resumeDiagnosis(ctx, resumeOptions{RunID: runID, RunDir: c.config.RunDir, SessionDir: c.config.SessionDir, MemoryDir: c.config.MemoryDir, Environment: c.config.Environment, OverwriteSessionMemory: c.config.OverwriteSessionMemory, ConfigPath: c.config.ConfigPath, Progress: progress, ResumeRunning: resumeRunning}, c.options.MockScenario)
	collected, err := saveDiagnosisResultWithCollection(result, runErr)
	c.recordInteractiveResult(result)
	completion := tuiCompletion(result, c.currentSession, collected, err)
	completion.Memories = loaded
	completion.MemoryLoaded = memoryLoaded
	return completion
}
func tuiCompletion(result diagnoseResult, sessionID string, collected bool, err error) tui.Completion {
	completion := tui.Completion{RunID: result.State.RunID, SessionID: sessionID, Status: string(result.State.Status), Collected: collected, Err: err}
	if result.State.Diagnosis != nil {
		d := result.State.Diagnosis
		completion.Summary = tools.RedactSensitive(d.Summary)
		completion.Evidence = d.Evidence
		completion.Recommendations = d.Recommendations
		completion.Health, completion.HealthDetail, completion.Issue, completion.Cause = tuiAssessment(result)
	}
	if err == nil {
		_, reportErr := markdownOutput("", result)
		if reportErr != nil {
			completion.Err = fmt.Errorf("Diagnosis finished, but report save failed: %w", reportErr)
		}
		if result.ReportDir != "" {
			completion.ReportPath = filepath.Join(result.ReportDir, result.State.RunID+".md")
		}
	}
	return completion
}

// tuiAssessment only uses current, persisted observations for health and affected area.
// Historical memory and free-form model text do not become health evidence.
func tuiAssessment(result diagnoseResult) (health, detail, issue, cause string) {
	health = "Unverified"
	diagnosis := result.State.Diagnosis
	if diagnosis == nil {
		return
	}
	seenDirect := false
	allDirectHealthy := true
	statusRank := 0
	for _, entry := range result.State.Trace {
		if !directBackendCheck(entry.ToolName) {
			continue
		}
		seenDirect = true
		if entry.Result.CheckStatus != schema.CheckExecutionCompleted {
			allDirectHealthy = false
			continue
		}
		switch entry.Result.TargetHealth {
		case schema.TargetHealthUnhealthy:
			if statusRank < 3 {
				statusRank = 3
				detail = entry.Result.Summary
			}
		case schema.TargetHealthDegraded:
			if statusRank < 2 {
				statusRank = 2
				detail = entry.Result.Summary
			}
		case schema.TargetHealthHealthy:
			if statusRank < 1 {
				statusRank = 1
				detail = entry.Result.Summary
			}
		default:
			allDirectHealthy = false
		}
	}
	switch {
	case statusRank == 3:
		health = "Unhealthy"
	case statusRank == 2:
		health = "Degraded"
	case seenDirect && allDirectHealthy && statusRank == 1:
		health = "Healthy in checked scope"
	default:
		detail = ""
	}
	// Prefer a concrete failing dependency check over a generic log line.
	for pass := 0; pass < 2 && issue == ""; pass++ {
		for _, e := range diagnosis.Evidence {
			if directBackendCheck(e.Tool) {
				continue
			}
			priority := strings.Contains(e.Tool, "postgres") || strings.Contains(e.Tool, "redis") || strings.Contains(e.Tool, "kafka") || strings.Contains(e.Tool, "database")
			if pass == 0 && !priority {
				continue
			}
			for _, entry := range result.State.Trace {
				if entry.Step != e.Step || entry.ToolName != e.Tool {
					continue
				}
				if entry.Result.TargetHealth == schema.TargetHealthUnhealthy || entry.Result.TargetHealth == schema.TargetHealthDegraded || entry.Result.CheckStatus == schema.CheckExecutionFailed || entry.Result.Error != "" {
					issue = e.Summary
					if issue == "" {
						issue = entry.Result.Summary
					}
					break
				}
			}
			if issue != "" {
				break
			}
		}
	}
	if issue == "" && health == "Unhealthy" {
		issue = detail
	}
	if diagnosis.RootCause != nil {
		switch diagnosis.RootCause.Status {
		case "identified":
			cause = diagnosis.RootCause.Statement
		case "suspected":
			cause = "Suspected: " + diagnosis.RootCause.Statement
		default:
			cause = "Not confirmed"
		}
	}
	return
}

func directBackendCheck(tool string) bool {
	switch tool {
	case "http_check", "websocket_check", "demo_http":
		return true
	default:
		return false
	}
}

func (b *tuiBackend) Command(ctx context.Context, line string) tui.CommandResult {
	c := b.cli
	previousSession := c.currentSession
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "/diagnose") {
		fields, err := splitInteractiveCommand(trimmed)
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Invalid command arguments: %v", err)}
		}
		if len(fields) < 2 || fields[0] != "/diagnose" {
			return tui.CommandResult{Text: "Usage: /diagnose <goal>"}
		}
		return tui.CommandResult{DiagnoseGoal: strings.Join(fields[1:], " ")}
	}
	if trimmed == "/llm ping" {
		content, err := pingLLM(ctx)
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("LLM ping failed: %v", err)}
		}
		return tui.CommandResult{Text: content, Title: "Model connection"}
	}
	if strings.HasPrefix(trimmed, "/llm chat ") {
		content, err := chatWithLLM(ctx, strings.TrimSpace(strings.TrimPrefix(trimmed, "/llm chat ")))
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("LLM chat failed: %v", err)}
		}
		return tui.CommandResult{Text: content, Title: "Model debugging"}
	}
	if trimmed == "/eval mock" || strings.HasPrefix(trimmed, "/eval mock ") {
		fields := strings.Fields(trimmed)
		scenario := "all"
		if len(fields) > 3 {
			return tui.CommandResult{Text: "Usage: /eval mock [scenario]"}
		}
		if len(fields) == 3 {
			scenario = fields[2]
		}
		results, err := runMockEvaluations(ctx, scenario, defaultEvaluationResultsDir)
		data, _ := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Mock evaluation failed: %v", err)}
		}
		return tui.CommandResult{Text: string(data), Title: "Offline evaluation results"}
	}
	if c.pending != nil && c.pending.kind == pendingEvalModel && strings.EqualFold(trimmed, "yes") {
		scenario := c.pending.scenario
		c.pending = nil
		result, err := c.deps.modelEvaluation(ctx, modelEvaluationOptions{Scenario: scenario, ConfigPath: c.config.ConfigPath, ResultsDir: defaultEvaluationResultsDir, ExecuteRealModel: true})
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Model evaluation failed: %v", err)}
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		return tui.CommandResult{Text: string(data), Title: "Model evaluation results"}
	}
	if trimmed == "/help" {
		descriptions := map[string]string{"/help": "Command help", "/sessions": "Find sessions", "/use": "Switch session", "/runs": "View runs", "/resume": "Resume run", "/status": "Run status", "/plan": "Check plan", "/evidence": "Check evidence", "/report": "Full report", "/config": "Effective config", "/memory": "Memory search and maintenance", "/eval": "Evaluation", "/llm": "Model debugging", "/new": "New session", "/exit": "Exit"}
		choices := make([]tui.Choice, 0, len(descriptions))
		for _, name := range []string{"/help", "/sessions", "/use", "/runs", "/resume", "/status", "/plan", "/evidence", "/report", "/config", "/memory", "/eval", "/llm", "/new", "/exit"} {
			choices = append(choices, tui.Choice{Value: strings.TrimPrefix(name, "/"), Label: name + "  " + descriptions[name]})
		}
		return tui.CommandResult{Title: "Commands", Text: "Type to filter, Up/Down to select, Enter to open.", Choices: choices, ChoiceCommand: "/help"}
	}
	if strings.HasPrefix(trimmed, "/help ") {
		topic := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "/help")), "/")
		usage := map[string]string{
			"memory": "/memory search <query> | collect [run_id] | rebuild | invalidate <run_id> | correct <run_id> | delete <run_id>",
			"llm":    "/llm ping | /llm chat <message> (direct model debugging; no diagnosis run)",
			"eval":   "/eval mock [scenario] | /eval model [scenario] (real model requires confirmation)",
			"resume": "/resume [run_id] (running runs require confirmation)",
		}
		if description, ok := usage[topic]; ok {
			return tui.CommandResult{Title: "/help " + topic, Text: description}
		}
		return tui.CommandResult{Title: "/help", Text: "Use /help to browse available commands."}
	}
	if trimmed == "/config" {
		path := c.config.ConfigPath
		if strings.TrimSpace(path) == "" {
			path = "configs/config.yaml"
		}
		return tui.CommandResult{Title: "Effective config", Text: fmt.Sprintf("Config file: %s\nEnvironment: %s\nService: %s\nModel: %s\nRun directory: %s\nSession directory: %s\nMemory directory: %s", path, c.config.Environment, c.config.Service, c.modelLabel, c.config.RunDir, c.config.SessionDir, c.config.MemoryDir)}
	}
	if strings.HasPrefix(trimmed, "/evidence-detail ") {
		fields := strings.Fields(trimmed)
		if len(fields) != 4 {
			return tui.CommandResult{Text: "Invalid evidence selection."}
		}
		step, err := strconv.Atoi(fields[2])
		if err != nil {
			return tui.CommandResult{Text: "Invalid evidence step."}
		}
		state, err := c.runStore.Load(fields[1])
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Could not load run: %v", err)}
		}
		for _, entry := range state.Trace {
			if entry.Step == step && entry.ToolName == fields[3] {
				data, err := json.MarshalIndent(entry.Result, "", "  ")
				if err != nil {
					return tui.CommandResult{Text: fmt.Sprintf("Could not encode evidence: %v", err)}
				}
				return tui.CommandResult{Title: "Evidence detail · " + entry.ToolName, Text: "Run: " + state.RunID + "\nCall: " + entry.CallID + "\n\n" + string(data)}
			}
		}
		return tui.CommandResult{Text: "Evidence is absent from this run trace."}
	}
	if trimmed == "/evidence" || strings.HasPrefix(trimmed, "/evidence ") {
		fields := strings.Fields(trimmed)
		id := c.latestRun
		if len(fields) > 2 {
			return tui.CommandResult{Text: "Usage: /evidence [run_id]"}
		}
		if len(fields) == 2 {
			id = fields[1]
		}
		if id == "" {
			return tui.CommandResult{Text: "No run is available."}
		}
		state, err := c.runStore.Load(id)
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Could not load run: %v", err)}
		}
		if state.Diagnosis == nil {
			return tui.CommandResult{Text: "This run has no final diagnosis or evidence."}
		}
		choices := make([]tui.Choice, 0, len(state.Diagnosis.Evidence))
		for _, e := range state.Diagnosis.Evidence {
			choices = append(choices, tui.Choice{Value: fmt.Sprintf("%s %d %s", id, e.Step, e.Tool), Label: fmt.Sprintf("Step %d · %s · %s", e.Step, e.Tool, e.Summary)})
		}
		if len(choices) == 0 {
			return tui.CommandResult{Text: "This diagnosis cites no evidence."}
		}
		return tui.CommandResult{Title: "Check evidence", Text: "Select evidence to view the redacted observation.", Choices: choices, ChoiceCommand: "/evidence-detail"}
	}
	if trimmed == "/status" || strings.HasPrefix(trimmed, "/status ") || trimmed == "/plan" || strings.HasPrefix(trimmed, "/plan ") {
		fields := strings.Fields(trimmed)
		if len(fields) > 2 {
			return tui.CommandResult{Text: "Usage: /status [run_id] or /plan [run_id]"}
		}
		id := c.latestRun
		if len(fields) == 2 {
			id = fields[1]
		}
		if id == "" {
			return tui.CommandResult{Text: "No run is available."}
		}
		state, err := c.runStore.Load(id)
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Could not load run: %v", err)}
		}
		if fields[0] == "/status" {
			content := fmt.Sprintf("Run: %s\nSession: %s\nStatus: %s\nGoal: %s\nUpdated: %s\nChecks: %d", state.RunID, displaySessionID(state.SessionID), state.Status, state.Goal, state.UpdatedAt.Local().Format("2006-01-02 15:04:05"), len(state.Trace))
			if state.Error != "" {
				content += "\nError: " + state.Error
			}
			return tui.CommandResult{Title: "Run status", Text: content}
		}
		var builder strings.Builder
		for _, item := range state.Plan.Items {
			fmt.Fprintf(&builder, "• [%s] %s\n  %s\n", item.Status, item.Goal, item.Reason)
		}
		if builder.Len() == 0 {
			builder.WriteString("No plan items were saved for this run.")
		}
		return tui.CommandResult{Title: "Check plan", Text: builder.String()}
	}
	if strings.HasPrefix(trimmed, "/resume ") {
		fields := strings.Fields(trimmed)
		if len(fields) != 2 {
			return tui.CommandResult{Text: "Usage: /resume [run_id]"}
		}
		state, err := c.runStore.Load(fields[1])
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Could not load run: %v", err)}
		}
		if !resumableStatus(state.Status) || state.Diagnosis != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Run %s cannot be resumed; use /report to view the result.", state.RunID)}
		}
		if state.Status == "running" {
			return tui.CommandResult{Text: fmt.Sprintf("Run %s is still marked running. Confirm that its original process has stopped before resuming.", state.RunID), Confirm: true, ResumeID: state.RunID, ResumeRunning: true}
		}
		return tui.CommandResult{ResumeID: state.RunID}
	}
	if trimmed == "/sessions" || trimmed == "/use" {
		states, err := c.sessionStore.List()
		if err != nil {
			return tui.CommandResult{Text: fmt.Sprintf("Could not load sessions: %v", err)}
		}
		choices := make([]tui.Choice, 0, len(states))
		for _, state := range states {
			summary := state.Goal
			if len(state.RunIDs) > 0 {
				if run, err := c.runStore.Load(state.RunIDs[len(state.RunIDs)-1]); err == nil && run.Diagnosis != nil {
					summary = run.Diagnosis.Summary
				}
			}
			choices = append(choices, tui.Choice{Value: state.SessionID, Label: fmt.Sprintf("%s  %s  %s  %s", state.SessionID, state.UpdatedAt.Local().Format("01-02 15:04"), state.Environment, summary)})
		}
		if len(choices) == 0 {
			return tui.CommandResult{Text: "No saved sessions."}
		}
		return tui.CommandResult{Title: "Select session", Text: "Type to filter, Up/Down to select, Enter to switch.", Choices: choices, ChoiceCommand: "/use"}
	}
	if trimmed == "/runs" || trimmed == "/resume" {
		var choices []tui.Choice
		if trimmed == "/resume" {
			states, err := c.resumableRuns()
			if err != nil {
				return tui.CommandResult{Text: fmt.Sprintf("Could not load run: %v", err)}
			}
			for _, state := range states {
				choices = append(choices, tui.Choice{Value: state.RunID, Label: fmt.Sprintf("%s  %s  %s", state.RunID, state.Status, state.Goal)})
			}
		} else if !c.newSession {
			session, err := c.sessionStore.Load(c.currentSession)
			if err != nil {
				return tui.CommandResult{Text: fmt.Sprintf("Could not load sessions: %v", err)}
			}
			for _, id := range session.RunIDs {
				state, err := c.runStore.Load(id)
				if err == nil {
					resumable := "not resumable"
					if resumableStatus(state.Status) && state.Diagnosis == nil {
						resumable = "resumable"
					}
					choices = append(choices, tui.Choice{Value: id, Label: fmt.Sprintf("%s  %s  %s  %s", id, state.Status, resumable, state.Goal)})
				}
			}
		}
		if len(choices) == 0 {
			return tui.CommandResult{Text: "No runs available."}
		}
		action := "/status"
		if trimmed == "/resume" {
			action = "/resume"
		}
		return tui.CommandResult{Title: "Select run", Text: "Type to filter, Up/Down to select, Enter to open.", Choices: choices, ChoiceCommand: action}
	}
	var buffer bytes.Buffer
	previous := c.output
	previousProgress := c.progress
	c.output = newTerminalWriter(&buffer)
	c.progress = newCLIProgressWriter(c.output)
	exit := c.handleLine(line)
	c.output = previous
	c.progress = previousProgress
	text := strings.TrimSpace(buffer.String())
	if !strings.HasPrefix(trimmed, "/report") && !strings.HasPrefix(trimmed, "/memory search") {
		text = tuiCommandText(text)
	}
	if trimmed == "/report" || strings.HasPrefix(trimmed, "/report ") {
		id := c.latestRun
		fields := strings.Fields(trimmed)
		if len(fields) == 2 {
			id = fields[1]
		}
		if id != "" {
			if cfg, err := resolveDiagnosisConfig(c.config); err == nil && cfg.ReportDir != "" {
				path := filepath.Join(cfg.ReportDir, id+".md")
				if _, err := os.Stat(path); err == nil {
					text = "Report file: " + path + "\n\n" + text
				}
			}
		}
	}
	result := tui.CommandResult{Text: text, SessionID: c.currentSession, Pending: c.pending != nil, Exit: exit}
	if c.currentSession != previousSession {
		result.ResetTranscript = true
		result.History = b.History()
	}
	if c.pending != nil {
		switch c.pending.kind {
		case pendingResumeRunning, pendingEvalModel, pendingMemoryDeleteConfirm:
			result.Confirm = true
		}
	}
	if !result.Pending && strings.HasPrefix(line, "/") {
		switch strings.Fields(line)[0] {
		case "/help", "/sessions", "/runs", "/status", "/plan", "/evidence", "/report", "/config", "/memory":
			result.Title = strings.Fields(line)[0]
		}
	}
	return result
}

// Translate fixed prompts inherited from the plain CLI. Persisted goals, evidence,
// model output and report content remain in their original language.
func tuiCommandText(value string) string {
	replacements := []struct{ old, new string }{
		{"请输入失效原因：", "Enter the invalidation reason:"},
		{"请输入修正后的结论强度（identified、suspected、undetermined、not_recorded）：", "Enter the corrected conclusion status (identified, suspected, undetermined, not_recorded):"},
		{"请输入修正说明：", "Enter the correction note:"},
		{"请输入逻辑删除原因：", "Enter the deletion reason:"},
		{"记忆索引已重建。", "Memory index rebuilt."},
		{"已收录记忆：", "Memory collected: "},
		{"该运行不满足记忆收录条件。", "This run does not meet memory collection criteria."},
		{"收录记忆失败：", "Could not collect memory: "},
		{"重建记忆索引失败：", "Could not rebuild memory index: "},
		{"/memory collect 需要 run_id；当前没有最近运行。", "/memory collect requires a run_id; no recent run exists."},
		{"结论强度必须是 identified、suspected、undetermined 或 not_recorded，已取消。", "Conclusion status must be identified, suspected, undetermined, or not_recorded; action cancelled."},
		{"已将记忆 ", "Marked memory "},
		{" 标记为失效。", " invalid."},
		{"已修正记忆 ", "Corrected memory "},
		{"，结论强度为 ", "; conclusion status: "},
		{"已从检索中逻辑删除记忆 ", "Removed memory from search for "},
		{"；原始 run 和复盘仍保留。", "; the original run remains available."},
		{"创建新会话失败：", "Could not create session: "},
		{"切换会话失败：", "Could not switch session: "},
		{"不能切换。", "cannot be selected."},
		{" 属于环境 ", " belongs to environment "},
		{"，当前环境是 ", "; current environment is "},
		{"命令参数错误：", "Invalid command arguments: "},
		{"未知交互命令：", "Unknown command: "},
		{"。输入 /help 查看可用命令。", ". Type /help to view commands."},
		{"读取报告失败：", "Could not load report: "},
		{"读取运行 ", "Could not load run "},
		{" 失败：", " failed: "},
		{" 需要 run_id；当前会话还没有最近运行。", " requires a run_id; this session has no recent run."},
		{"没有 /", "No dedicated help for /"},
		{"用法：", "Usage: "},
		{"未知 /memory 命令；可用命令：", "Unknown /memory command. Available: "},
		{"未知 /eval 命令；可用命令：", "Unknown /eval command. Available: "},
		{"未知 /llm 命令；可用命令：", "Unknown /llm command. Available: "},
		{"搜索记忆失败：", "Memory search failed: "},
		{"标记记忆失效失败：", "Could not invalidate memory: "},
		{"修正记忆失败：", "Could not correct memory: "},
		{"删除记忆失败：", "Could not delete memory: "},
		{"失效原因不能为空，已取消。", "Reason is required; action cancelled."},
		{"修正说明不能为空，已取消。", "Correction note is required; action cancelled."},
		{"删除原因不能为空，已取消。", "Deletion reason is required; action cancelled."},
		{"未确认删除，已取消。", "Deletion cancelled."},
		{"会话 ", "Session "},
		{"已切换到会话：", "Switched to session: "},
		{"已创建新会话：", "Created session: "},
		{"。跨会话记忆仍会自动检索。", ". Cross-session memory remains automatic."},
		{"将从检索中逻辑删除记忆 ", "Remove memory from search for "},
		{"，原因：", "; reason: "},
		{"。输入 yes 确认，其他输入取消。", ". Select Confirm to delete or Cancel to keep it."},
		{"真实模型评测 ", "Real model evaluation "},
		{" 可能产生费用。输入 yes 明确授权；其他输入取消且不会发起模型请求。", " may incur charges. Select Confirm to run it or Cancel to stop."},
	}
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement.old, replacement.new)
	}
	return value
}

func (b *tuiBackend) CancelPending() { b.cli.pending = nil }
