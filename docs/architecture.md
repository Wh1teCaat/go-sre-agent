# 架构

`go-sre-agent` 是一个 CLI-first 的 SRE 诊断 Agent，不使用 Agent 框架。Runtime 向 LLM provider 请求下一步结构化 action，校验 action，执行白名单内的只读工具，记录 trace，然后持续循环，直到 provider 返回最终诊断。

## 模块

- `internal/agent`: runtime loop, state, prompt boundary, execution boundary.
- `internal/llm`: provider interface, action planner, generic chat types/config, mock provider, OpenAI-compatible/Ollama client, and Anthropic Messages client.
- `internal/tools`: tool interface, registry, tool specs, and concrete tool packages.
- `internal/policy`: action validation, tool schema arg validation, max-step policy, allowlist and timeout settings.
- `internal/trace`: per-step execution trace storage.
- `internal/report`: markdown report generation from diagnosis and trace evidence.
- `internal/schema`: structured action, observation, evidence, and diagnosis types.

## Runtime Loop

1. Receive a user goal from CLI.
2. Derive LLM-facing observations from the trace store.
3. Send goal, available tool specs, and observations to the LLM provider.
4. In real LLM mode, `ActionPlanner` turns the diagnostic context into a generic `ChatRequest`.
5. The configured `ChatClient` translates the generic chat request into the model provider's HTTP API.
6. `ActionPlanner` parses the model content into a structured action. It can recover JSON from a Markdown code fence and reports a short content preview when parsing fails.
7. Validate the returned structured action, final summary, final evidence trace references, tool allowlist, and basic tool arg schema.
8. Execute the selected tool under a timeout.
9. If tool execution fails, record the failure as both trace error and an observation with an `error` field, then continue to the next planning step.
10. Append trace with step, thought summary, tool, args, result, error, and duration.
11. Stop on `final` or return an error after `max_steps`.

## 安全边界

- Runtime enforces max steps and per-tool timeout.
- Policy enforces final summary, tool allowlist, required/basic-type arg schema, and rejects unknown tool args when a schema is known.
- Runtime rejects final evidence whose step/tool pair does not exist in the current trace.
- Tool execution errors are preserved as LLM-facing observations instead of terminating the runtime loop.
- `log_read` only reads files under configured allowed directories.
- `http_check` body snippets and `log_read` lines redact common password/token/api_key/secret values before they become observations.
- `http_check` and `websocket_check` reject URLs whose host is not in configured `allowed_hosts`.
- Tool packages are independent and can be added without changing the runtime contract.
