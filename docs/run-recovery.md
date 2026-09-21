# 阶段 2：运行 checkpoint、取消与恢复

本阶段把 `.runs/<run_id>.json` 从“运行结束后的快照”扩展为可恢复的事实
记录。它仍是运行恢复、证据回查和报告重建的主来源；会话 Markdown 和记忆索引
不能替代它。

## 持久化模型

启动成功后，程序先以 `status: running` 原子保存 run JSON，随后在每一次 LLM
请求和工具调用的**开始前**、**获得结果后**同步 checkpoint。每个 `calls[]` 项有
独立的 `call_id`、类型、step/attempt、工具名和脱敏参数、状态、错误分类、结果、
开始/结束时间。工具 trace 会链接对应的 `call_id`。

调用状态含义：

- `running`：已写入调用意图，外部执行尚未有可靠结果。
- `succeeded` / `failed` / `cancelled`：本进程已收到可分类的结果。
- `unknown`：调用可能已到达目标，但结果不可确认。

每次 checkpoint 都采用现有的同目录临时文件加原子替换；checkpoint 写入失败会
阻止下一次外部调用。即使会话 Markdown 更新失败，原始 run JSON 也已经单独保存。

## 取消和总时间预算

`diagnose` 与 `resume` 监听 Ctrl-C (`SIGINT`) 和 `SIGTERM`。首次信号取消运行
上下文，runtime 在当前调用返回后写入 `cancelled` 终态；终端仍会返回非零退出码和
`run_id`，可用 `status` 查看已保存状态。

总预算默认五分钟，可在配置或 CLI 覆盖：

```yaml
agent:
  task_timeout: 5m
```

```bash
sre-agent diagnose --goal "检查登录接口" --task-timeout 2m
```

总预算到期会保存 `timed_out` 以及 `error_class: deadline_exceeded`。`llm_timeout`
和 `tool_timeout` 仍只限制单次调用，不能替代总预算。

若进程被强制杀死，或调用方不响应 context，最后一次已保存调用可能仍是
`running`；这不是成功或失败的证据。

## 恢复规则

可恢复的正常终态包括 `failed`、`cancelled` 和 `timed_out`。旧格式中缺少 calls 的
失败 run 也仍可恢复。

仍标为 `running` 的 run 可能仍被另一个进程执行，所以默认拒绝恢复。确认原进程已
停止后才可显式使用：

```bash
sre-agent resume --run-id <run_id> --resume-running
```

恢复前会将旧的 `running` 调用原子标记为 `unknown`。LLM 调用和只读探测的未知结果
不会被 runtime 当作已成功；模型会在新的执行上下文中决定后续检查。

`smoke_run` 标为 `side_effect: true`。如果它在中断、超时或报错时结果不确定，run
将包含未知副作用调用，`resume` 会无条件拒绝自动恢复，且没有“忽略并重试”的 flag。
操作者必须先核验目标环境和测试账号的实际状态；如仍需检查，应创建新的 run，而不是
让旧 run 自动重复潜在写入。

## 错误与重试边界

LLM 请求最多尝试两次。仅下列情况会进入第二次：模型输出校验失败、可识别的临时
网络错误/HTTP 408、429、5xx，或单次 LLM 超时且总任务预算尚未耗尽。永久错误（例如配置/认证错误）不
会自动重试。工具不由 runtime 自动重试，避免将外部检查误判为幂等操作；工具失败会
继续作为 observation 留给下一步诊断。

本阶段没有实现阶段 4 的并行调度、调用配额或实时进度事件；所有 checkpoint 调用按
单线程顺序执行。
