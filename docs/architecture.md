# 架构

`go-sre-agent` 是一个 CLI-first 的 SRE 诊断 Agent，不使用 Agent 框架。Runtime 先让 LLM provider 决定直接行动还是按需规划，校验 action，执行白名单内的诊断工具，记录 trace，然后持续循环，直到 provider 返回最终诊断。除显式配置的 `smoke_run` 外，工具均为只读；`smoke_run` 是受恢复限制的合成事务。

## 模块

- `cmd/sre-agent/init.go`: startup wiring for config, tools, LLM client/provider, skill, and model context.
- `cmd/sre-agent/diagnose.go`: start/resume orchestration, task deadline, and run checkpoint wiring.
- `cmd/sre-agent/run.go`: terminal run persistence, status lookup, and report reload.
- `cmd/sre-agent/commands.go`: CLI flag parsing, exit handling, and output.
- `cmd/sre-agent/interactive*.go`: 默认 `sre` 逐行交互入口、会话状态、局部命令分派、进度渲染和取消处理；它直接复用诊断、运行、会话、记忆和评测应用函数，不通过子进程再次调用 CLI。
- `cmd/sre-agent/memory_command.go`: 固定 `memories/` 根目录的收录、检索、重建和生命周期命令。
- `internal/agent`: runtime loop, state, prompt boundary, execution boundary.
- `internal/llm`: provider interface, action planner, generic chat types/config, mock provider, OpenAI-compatible/Ollama client, and Anthropic Messages client.
- `internal/tools`: tool interface, registry, tool specs, and concrete tool packages.
- `internal/policy`: action、计划、结构化证据、根因约束、工具白名单和参数 schema 校验。
- `internal/trace`: per-step execution trace storage.
- `internal/report`: markdown report generation from diagnosis and trace evidence.
- `internal/memory`: 从已保存 run 确定性生成跨会话复盘、主题索引和受预算限制的历史提示。
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

`sre` 无子命令时仅在终端标准输入下进入交互会话；管道或重定向会提示使用脚本子命令并退出。交互状态保存当前配置、环境、会话 ID、最近 run ID 和任务状态，终端文案不是状态来源。普通文本创建新 run，`/` 命令由本地解析器处理；历史会话和跨会话 memory 仍由同一诊断路径按预算自动加载与收录。进度事件写入 stderr，报告与结构化状态写入 stdout，二者共享安全的终端渲染边界。

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
- Runtime publishes structured progress events. The CLI renders those events to stderr, while reports and machine-readable output remain on stdout.
- 跨会话索引采用原子替换和短时写锁；索引故障不会删除已经保存的 run，且历史 Markdown 不能改变 system、工具或证据策略。
- `log_read` only reads files under configured allowed directories.
- `http_check` body snippets and `log_read` lines redact common password/token/api_key/secret values before they become observations.
- `http_check` and `websocket_check` reject URLs whose host is not in configured `allowed_hosts`.
- Tool packages are independent and can be added without changing the runtime contract.
