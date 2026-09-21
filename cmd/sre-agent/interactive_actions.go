package main

import (
	"context"
	"fmt"
	"strings"

	memory "github.com/y2/go-sre-agent/internal/memory"
)

// handlePending 处理只在选择、授权或破坏性记忆操作时创建的下一行交互状态。
func (c *interactiveCLI) handlePending(line string) bool {
	pending := c.pending
	c.pending = nil
	line = strings.TrimSpace(line)
	switch pending.kind {
	case pendingResumeSelection:
		if line == "" {
			fmt.Fprintln(c.output, "已取消恢复选择。")
			return false
		}
		if _, ok := pending.candidates[line]; !ok {
			fmt.Fprintln(c.output, "该 run 不在刚才列出的可恢复记录中，已取消。")
			return false
		}
		return c.prepareResume(line)
	case pendingResumeRunning:
		if !confirmed(line) {
			fmt.Fprintln(c.output, "未确认恢复 running run，已取消。")
			return false
		}
		return c.runResume(pending.runID, true)
	case pendingEvalModel:
		if !confirmed(line) {
			fmt.Fprintln(c.output, "未授权真实模型评测，未发起模型请求。")
			return false
		}
		return c.runModelEvaluation(pending.scenario)
	case pendingMemoryInvalidate:
		if line == "" {
			fmt.Fprintln(c.output, "失效原因不能为空，已取消。")
			return false
		}
		if err := c.memoryStore.Invalidate(pending.runID, line); err != nil {
			fmt.Fprintf(c.output, "标记记忆失效失败：%v\n", err)
			return false
		}
		fmt.Fprintf(c.output, "已将记忆 %s 标记为失效。\n", pending.runID)
	case pendingMemoryCorrectStatus:
		if !interactiveConclusionStatus(line) {
			fmt.Fprintln(c.output, "结论强度必须是 identified、suspected、undetermined 或 not_recorded，已取消。")
			return false
		}
		c.pending = &interactivePending{kind: pendingMemoryCorrectNote, runID: pending.runID, status: line}
		fmt.Fprintln(c.output, "请输入修正说明：")
	case pendingMemoryCorrectNote:
		if line == "" {
			fmt.Fprintln(c.output, "修正说明不能为空，已取消。")
			return false
		}
		if err := c.memoryStore.Correct(pending.runID, pending.status, line); err != nil {
			fmt.Fprintf(c.output, "修正记忆失败：%v\n", err)
			return false
		}
		fmt.Fprintf(c.output, "已修正记忆 %s，结论强度为 %s。\n", pending.runID, pending.status)
	case pendingMemoryDeleteReason:
		if line == "" {
			fmt.Fprintln(c.output, "删除原因不能为空，已取消。")
			return false
		}
		c.pending = &interactivePending{kind: pendingMemoryDeleteConfirm, runID: pending.runID, reason: line}
		fmt.Fprintf(c.output, "将从检索中逻辑删除记忆 %s，原因：%s。输入 yes 确认，其他输入取消。\n", pending.runID, line)
	case pendingMemoryDeleteConfirm:
		if !confirmed(line) {
			fmt.Fprintln(c.output, "未确认删除，已取消。")
			return false
		}
		if err := c.memoryStore.Delete(pending.runID, pending.reason); err != nil {
			fmt.Fprintf(c.output, "删除记忆失败：%v\n", err)
			return false
		}
		fmt.Fprintf(c.output, "已从检索中逻辑删除记忆 %s；原始 run 和复盘仍保留。\n", pending.runID)
	default:
		fmt.Fprintln(c.output, "交互状态无效，已取消。")
	}
	return false
}

// confirmed 只接受明确的 yes，避免含糊输入触发恢复、收费评测或删除操作。
func confirmed(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "yes")
}

// interactiveConclusionStatus 限定交互修正可传给 memory 存储的结论强度集合。
func interactiveConclusionStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "identified", "suspected", "undetermined", "not_recorded":
		return true
	default:
		return false
	}
}

// runLLM 提供显式的底层模型调试入口；它不创建诊断 run，也不应与普通文本诊断混淆。
func (c *interactiveCLI) runLLM(arguments []string) bool {
	if len(arguments) == 0 {
		fmt.Fprintln(c.output, "用法：/llm ping 或 /llm chat <消息>")
		return false
	}
	var content string
	var taskErr error
	switch arguments[0] {
	case "ping":
		if len(arguments) != 1 {
			fmt.Fprintln(c.output, "用法：/llm ping")
			return false
		}
		terminate := c.runTask(func(ctx context.Context) { content, taskErr = pingLLM(ctx) })
		if taskErr != nil {
			fmt.Fprintf(c.output, "LLM ping 失败：%v\n", taskErr)
		} else {
			fmt.Fprintln(c.output, content)
		}
		return terminate
	case "chat":
		if len(arguments) < 2 {
			fmt.Fprintln(c.output, "用法：/llm chat <消息>")
			return false
		}
		message := strings.Join(arguments[1:], " ")
		terminate := c.runTask(func(ctx context.Context) { content, taskErr = chatWithLLM(ctx, message) })
		if taskErr != nil {
			fmt.Fprintf(c.output, "LLM chat 失败：%v\n", taskErr)
		} else {
			fmt.Fprintln(c.output, content)
		}
		return terminate
	default:
		fmt.Fprintln(c.output, "未知 /llm 命令；可用命令：ping、chat。")
		return false
	}
}

// runEval 分派离线 mock 评测和需要交互确认的真实模型评测。
func (c *interactiveCLI) runEval(arguments []string) bool {
	if len(arguments) == 0 {
		fmt.Fprintln(c.output, "用法：/eval mock [scenario] 或 /eval model [scenario]")
		return false
	}
	scenario := ""
	if len(arguments) > 2 {
		fmt.Fprintln(c.output, "用法：/eval mock [scenario] 或 /eval model [scenario]")
		return false
	}
	if len(arguments) == 2 {
		scenario = arguments[1]
	}
	switch arguments[0] {
	case "mock":
		if scenario == "" {
			scenario = "all"
		}
		return c.runMockEvaluations(scenario)
	case "model":
		if scenario == "" {
			scenario = "login-500"
		}
		c.pending = &interactivePending{kind: pendingEvalModel, scenario: scenario}
		fmt.Fprintf(c.output, "真实模型评测 %s 可能产生费用。输入 yes 明确授权；其他输入取消且不会发起模型请求。\n", scenario)
		return false
	default:
		fmt.Fprintln(c.output, "未知 /eval 命令；可用命令：mock、model。")
		return false
	}
}

// runMockEvaluations 调用现有离线评测服务，不读取真实模型配置或目标。
func (c *interactiveCLI) runMockEvaluations(scenario string) bool {
	var results []storedEvaluation
	var taskErr error
	terminate := c.runTask(func(ctx context.Context) {
		results, taskErr = runMockEvaluations(ctx, scenario, defaultEvaluationResultsDir)
	})
	c.printJSON(results)
	if taskErr != nil {
		fmt.Fprintf(c.output, "mock 评测失败：%v\n", taskErr)
	} else {
		fmt.Fprintln(c.output, "mock 评测完成。")
	}
	return terminate
}

// runModelEvaluation 只在 pendingEvalModel 收到 yes 后执行，沿用既有显式授权结果记录。
func (c *interactiveCLI) runModelEvaluation(scenario string) bool {
	var result storedEvaluation
	var taskErr error
	terminate := c.runTask(func(ctx context.Context) {
		result, taskErr = c.deps.modelEvaluation(ctx, modelEvaluationOptions{
			Scenario:         scenario,
			ConfigPath:       c.config.ConfigPath,
			ResultsDir:       defaultEvaluationResultsDir,
			ExecuteRealModel: true,
		})
	})
	if result.Result.ID != "" {
		c.printJSON(result)
	}
	if taskErr != nil {
		fmt.Fprintf(c.output, "真实模型评测失败：%v\n", taskErr)
	} else {
		fmt.Fprintln(c.output, "真实模型评测完成。")
	}
	return terminate
}

// runMemory 提供维护入口；普通诊断无需先执行这些命令，memory 会自动检索和收录。
func (c *interactiveCLI) runMemory(arguments []string) bool {
	if len(arguments) == 0 {
		fmt.Fprintln(c.output, "用法：/memory search|collect|rebuild|invalidate|correct|delete ...")
		return false
	}
	switch arguments[0] {
	case "search":
		if len(arguments) < 2 {
			fmt.Fprintln(c.output, "用法：/memory search <关键词>")
			return false
		}
		matches, err := c.memoryStore.Search(memory.Query{
			Service:     c.config.Service,
			Environment: c.config.Environment,
			Goal:        strings.Join(arguments[1:], " "),
			MaxMatches:  3,
			MaxBytes:    12 * 1024,
		})
		if err != nil {
			fmt.Fprintf(c.output, "搜索记忆失败：%v\n", err)
			return false
		}
		c.printJSON(matches)
	case "collect":
		if len(arguments) > 2 {
			fmt.Fprintln(c.output, "用法：/memory collect [run_id]")
			return false
		}
		runID := c.latestRun
		if len(arguments) == 2 {
			runID = arguments[1]
		}
		if strings.TrimSpace(runID) == "" {
			fmt.Fprintln(c.output, "/memory collect 需要 run_id；当前没有最近运行。")
			return false
		}
		c.collectMemory(runID)
	case "rebuild":
		if len(arguments) != 1 {
			fmt.Fprintln(c.output, "用法：/memory rebuild")
			return false
		}
		if err := c.memoryStore.Rebuild(); err != nil {
			fmt.Fprintf(c.output, "重建记忆索引失败：%v\n", err)
		} else {
			fmt.Fprintln(c.output, "记忆索引已重建。")
		}
	case "invalidate":
		if len(arguments) != 2 {
			fmt.Fprintln(c.output, "用法：/memory invalidate <run_id>")
			return false
		}
		c.pending = &interactivePending{kind: pendingMemoryInvalidate, runID: arguments[1]}
		fmt.Fprintln(c.output, "请输入失效原因：")
	case "correct":
		if len(arguments) != 2 {
			fmt.Fprintln(c.output, "用法：/memory correct <run_id>")
			return false
		}
		c.pending = &interactivePending{kind: pendingMemoryCorrectStatus, runID: arguments[1]}
		fmt.Fprintln(c.output, "请输入修正后的结论强度（identified、suspected、undetermined、not_recorded）：")
	case "delete":
		if len(arguments) != 2 {
			fmt.Fprintln(c.output, "用法：/memory delete <run_id>")
			return false
		}
		c.pending = &interactivePending{kind: pendingMemoryDeleteReason, runID: arguments[1]}
		fmt.Fprintln(c.output, "请输入逻辑删除原因：")
	default:
		fmt.Fprintln(c.output, "未知 /memory 命令；可用命令：search、collect、rebuild、invalidate、correct、delete。")
	}
	return false
}

// collectMemory 为指定持久化 run 手动执行现有的确定性收录流程，不改变正常自动收录路径。
func (c *interactiveCLI) collectMemory(runID string) {
	state, err := c.runStore.Load(runID)
	if err != nil {
		fmt.Fprintf(c.output, "读取运行失败：%v\n", err)
		return
	}
	state, err = fillLegacyMemoryScope(state, c.config.ConfigPath)
	if err != nil {
		fmt.Fprintf(c.output, "补足旧运行范围失败：%v\n", err)
		return
	}
	collected, err := c.memoryStore.UpdateForRun(state)
	if err != nil {
		fmt.Fprintf(c.output, "收录记忆失败：%v\n", err)
		return
	}
	if !collected {
		fmt.Fprintln(c.output, "该运行不满足记忆收录条件。")
		return
	}
	fmt.Fprintf(c.output, "已收录记忆：%s\n", state.RunID)
}

// printHelp 输出所有交互命令，未知帮助主题不会改变会话状态。
func (c *interactiveCLI) printHelp(arguments []string) {
	if len(arguments) > 1 {
		fmt.Fprintln(c.output, "用法：/help [command]")
		return
	}
	if len(arguments) == 1 {
		switch strings.TrimPrefix(arguments[0], "/") {
		case "memory":
			fmt.Fprintln(c.output, "/memory search <关键词> | collect [run_id] | rebuild | invalidate <run_id> | correct <run_id> | delete <run_id>")
		case "llm":
			fmt.Fprintln(c.output, "/llm ping | /llm chat <消息>。这是直接模型调试，不会创建诊断 run。")
		case "eval":
			fmt.Fprintln(c.output, "/eval mock [scenario] | /eval model [scenario]。后者必须输入 yes 才会调用真实模型。")
		case "resume":
			fmt.Fprintln(c.output, "/resume [run_id]。不带 ID 时列出可恢复记录并要求选择；running run 仍需确认。")
		default:
			fmt.Fprintf(c.output, "没有 /%s 的专用帮助；可用命令如下。\n", arguments[0])
		}
	}
	fmt.Fprintln(c.output, "普通文本：创建新的诊断 run。")
	fmt.Fprintln(c.output, "/diagnose <问题>  /resume [run_id]  /status [run_id]  /report [run_id]")
	fmt.Fprintln(c.output, "/runs  /sessions  /use <session_id>  /new  /plan [run_id]  /evidence [run_id]  /config")
	fmt.Fprintln(c.output, "/llm ping|chat  /eval mock|model  /memory ...  /exit")
}
