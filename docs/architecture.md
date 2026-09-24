# 架构

`go-sre-agent` 是一个以单列 TUI 为默认入口的 SRE 诊断 Agent，同时保留逐行交互和非交互子命令，不使用 Agent 框架。Runtime 先让 LLM provider 决定直接行动还是按需规划，校验 action，执行白名单内的诊断工具，记录 trace，然后持续循环，直到 provider 返回最终诊断。除显式配置的 `smoke_run` 外，工具均为只读；`smoke_run` 是受恢复限制的合成事务。

## 模块

- `cmd/sre-agent/init.go`: startup wiring for config, tools, LLM client/provider, skill, and model context.
- `cmd/sre-agent/diagnose.go`: start/resume orchestration, task deadline, and run checkpoint wiring.
- `cmd/sre-agent/run.go`: terminal run persistence, status lookup, and report reload.
- `cmd/sre-agent/commands.go`: CLI flag parsing, exit handling, and output.
- `cmd/sre-agent/interactive*.go`: 保留的 `--plain` 逐行交互、会话状态和命令分派。
- `cmd/sre-agent/tui.go` 与 `internal/tui`: 将现有诊断、恢复、记忆和命令能力接入终端事件循环，渲染单列对话、工具进度、选择器和详情视图；后台任务只向 TUI 发送消息。
- `cmd/sre-agent/memory_command.go` 与 `memory_worker.go`: 固定 `memories/` 根目录的处理、检索、重建和生命周期命令；交互模式在后台逐项处理模型记忆。
- `internal/agent`: runtime loop, state, prompt boundary, execution boundary.
- `internal/llm`: provider interface, action planner, generic chat types/config, mock provider, OpenAI-compatible/Ollama client, and Anthropic Messages client.
- `internal/tools`: tool interface, registry, tool specs, and concrete tool packages.
- `internal/policy`: action、计划、结构化证据、根因约束、工具白名单和参数 schema 校验。
- `internal/trace`: per-step execution trace storage.
- `internal/report`: markdown report generation from diagnosis and trace evidence.
- `internal/memory`: 从已保存 run 确定性生成复盘和索引；可选模型提取单次候选经验、按服务与环境整合，校验来源及摘要后提供受预算限制的历史提示。
- `internal/schema`: structured action, observation, evidence, and diagnosis types.

## Runtime Loop

1. Receive a user goal from CLI.
2. Load only the selected session memory and a budget-bounded, service/environment-scoped cross-session history; both are historical hypotheses rather than current evidence.
3. Derive budget-bounded LLM-facing observation copies from the trace store; complete evidence remains in trace.
4. Send goal, available tool specs, historical hints, and observations to the LLM provider for a decision.
5. Continue directly for a tool/final action; call `Plan` only when the decision sets `NeedsPlan`, then request the action again in the same step.
6. Validate the returned structured action, plan adherence, evidence roles, conclusion strength, final evidence, tool allowlist, and tool args.
7. Before and after each LLM or tool request, atomically checkpoint the run, call ID, and call state.
8. Create a durable call ID for every selected tool. Independent read-only `tool_calls` batches run with bounded parallelism; trace/checkpoint writes remain serialized.
9. Tool failures return as observations; the next decision may request planning or continue normal ReAct.
10. Stop on `final`, cancellation, total-task timeout, or an error after `max_steps`.

## 交互入口

`sre` 无子命令且 stdin、stdout 都是可用终端时，默认进入全屏 TUI；`--plain` 使用逐行交互。输入被重定向时会提示使用脚本子命令并退出。普通文本创建新 run，`/` 命令由本地解析器处理；历史会话和跨会话记忆沿用同一诊断路径按预算自动加载与收录。Runtime 发布结构化进度事件，TUI 在单一事件循环中更新视图，后台任务不直接向终端打印；非交互子命令仍将进度写入 stderr、报告与结构化状态写入 stdout。

终态 Run 先保存到 `.runs/`，再同步更新会话与确定性跨会话复盘。若模型记忆开启，TUI worker 启动时扫描待办、保存新 Run 后接收通知；提取与整合调用期间只持有单项租约，提交时才短时锁住索引并复核输入摘要。worker 通过事件通道报告状态，退出时取消模型请求。`sre memory process` 使用同一处理器；`sre memory rebuild` 仅读取可验证的落盘结果。

## 安全边界

- Runtime enforces max steps and per-tool timeout.
- Runtime also enforces a total tool-call budget, bounded observation context, and a maximum of eight parallel read-only tools.
- The CLI supplies a whole-task deadline and turns Ctrl-C/SIGTERM into a persisted cancellation state.
- Every external call has a durable `call_id`; in-flight calls recovered after interruption become `unknown` rather than successful.
- A run with an unknown side-effecting `smoke_run` call cannot be resumed automatically.
- Policy enforces final summary, tool allowlist, required/basic-type arg schema, and rejects unknown tool args when a schema is known.
- Runtime rejects final evidence whose step/tool pair does not exist in the current trace, and keeps check execution status separate from target health.
- `identified` root causes require a supported fault type and its minimum structured evidence; `suspected` conclusions require both support and pending verification.
- Tool execution errors are preserved as LLM-facing observations instead of terminating the runtime loop.
- Runtime publishes structured progress events. The TUI consumes them in its event loop; non-interactive commands render progress to stderr and reports or machine-readable output to stdout.
- 跨会话索引采用原子替换和短时写锁；模型提取与整合文件含来源摘要并按当前 Run、服务、环境及生命周期复核，过期内容不注入。索引故障不会删除已保存的 Run，历史材料不能改变 system、工具或证据策略。
- `log_read` only reads files under configured allowed directories.
- `http_check` body snippets and `log_read` lines redact common password/token/api_key/secret values before they become observations.
- `http_check` and `websocket_check` reject URLs whose host is not in configured `allowed_hosts`.
- Tool packages are independent and can be added without changing the runtime contract.
