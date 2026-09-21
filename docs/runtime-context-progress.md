# 阶段 4：上下文预算、进度与有限并行

本阶段在不改写原始运行事实的前提下，为模型上下文和工具执行加入边界。完整
`trace`、工具结果和 `call_id` 仍保存于 `.runs/<run_id>.json`；预算只限制发送给
模型的观测副本，不能作为删除或压缩原始证据的机制。

## 上下文预算

下列配置同时可由 `diagnose` 和 `resume` 的同名 flag 覆盖：

```yaml
agent:
  # 一次 run 中可开始的工具调用总数，默认 24。
  max_tool_calls: 24
  # 同一批独立只读调用的最大并发数，范围为 1 到 8，默认 2。
  max_parallel_tools: 2
  # 所有观测副本的总字节预算，至少 1024，默认 49152。
  context_budget_bytes: 49152
  # 单条工具观测副本的字节预算，至少 512，默认 8192。
  tool_output_budget_bytes: 8192
```

runtime 从最新观测开始选择内容，以优先保留当前排障最相关的结果，随后按时间顺序
发送。单条输出过大时，仅传递状态、有限结构化事实、截断的摘要/错误和：

```json
{
  "context_truncated": true,
  "trace_reference": {"step": 3, "tool": "log_read", "call_id": "call_..."}
}
```

`trace_reference` 是回查原始证据的定位信息，不是新证据。模型只能依据可见字段提出
假设或继续检查，不能推断被省略的原始内容。报告、证据校验和恢复始终读取完整 trace。

## 有限并行

模型可在一个诊断 step 返回 `tool_calls`：

```json
{
  "type": "tool_calls",
  "tool_calls": [
    {"thought_summary": "检查 Redis", "plan_item_id": "redis", "tool": "redis_ping", "args": {}},
    {"thought_summary": "检查 PostgreSQL", "plan_item_id": "postgres", "tool": "postgres_ping", "args": {}}
  ]
}
```

批次必须至少有两项，且每项使用不同工具；有计划时还必须归属不同的 `plan_item_id`。
所有工具必须是只读，`smoke_run` 等可能有副作用的工具不能进入批次。依赖前一项输出、
需要按顺序比较，或可能改变目标状态的检查必须保留为单个 `tool_call`。runtime 不会把
多个普通 `tool_call` 自动合并为并行调用。

在执行前，runtime 会为批次内每项先创建独立的 `call_id` 并写入 `running`
checkpoint，然后按 `max_parallel_tools` 调度。worker 只执行工具并返回结果；trace、
调用终态和 checkpoint 由主循环按 action 顺序写入。因此并行 worker 不会竞争 run
持久化状态。批次每一项都消耗 `max_tool_calls` 预算。

如果取消、超时或 checkpoint 中断发生在批次内，已完成的结果按正常规则持久化；未能
可靠写回的 `running` 调用在恢复前会转为 `unknown`。带副作用的未知调用仍遵循阶段 2
的恢复阻断规则。

## CLI 进度与输出流

普通进度、`run_id` 和会话标识写入 stderr；Markdown 报告仍只写入 stdout。因此可以
安全地将报告重定向给文件或下游程序：

```bash
go run ./cmd/sre-agent diagnose \
  --mock-scenario dependency-check \
  --goal "检查依赖状态" \
  --max-parallel-tools 2 \
  --context-budget-bytes 49152 \
  > report.md
```

stderr 的正常输出格式如下：

```text
[1/12] 正在检查登录接口…
[1/12] 检查完成：HTTP 500，耗时 86ms
正在整理诊断报告…
诊断完成，报告已保存。
```

这些事件是 runtime 的结构化进度回调，包含 step、总步数、工具、计划项、独立
`call_id`、摘要/错误和耗时。进度回调不参与模型决策，也不替代 `.runs` 中的事实记录。

## 边界与兼容性

- 旧 run 不需要迁移；既有 trace 会在读取模型上下文时按同样预算处理。
- `tool_call`、原有单工具 checkpoint 和恢复行为保持兼容。
- 当前实现按元数据和显式 `tool_calls` 执行有限并行，不会依据自然语言自动判断可并行性。
- 本阶段不写入 `memories/`，也没有实现跨会话知识检索或索引。
