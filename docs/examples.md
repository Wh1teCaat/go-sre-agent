# 示例

## 真实 LLM 连通性测试

如果只想确认 `.env` 里的模型配置能否调用成功，可以直接测试底层 `ChatClient`：

```bash
sre-agent llm ping
```

也可以输入自己的消息：

```bash
sre-agent llm chat --message "用一句话说明 SRE 诊断 Agent 是什么"
```

这条路径只打印模型原始文本，不进入 Agent runtime，也不会调用工具。

## 真实 LLM 诊断烟测

```bash
sre-agent diagnose \
  --config configs/config.example.yaml \
  --goal "仅验证真实 ActionPlanner 和 ChatClient 接入，不调用工具，直接生成 final 诊断"
```

当 `.env` 设置了真实 provider 时，这条路径会使用 `ActionPlanner` 和对应 ChatClient。想跑本地固定链路时，加上 `--mock-scenario skeleton`。

## 登录接口 500

```bash
sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario login-500 \
  --goal "诊断 chat_proj 登录接口 /v1/user/login 为什么返回 500"
```

当前 mock 流程：

1. `http_check` 请求 `/v1/user/login`。
2. `log_read` 从最近日志中搜索堆栈、SQL 错误等线索。
3. 报告会基于实际 trace observation 展示 evidence，并过滤没有 trace 支撑的模型证据声明。

后续真实诊断流程会在第一轮证据之后，让模型按需要继续调用 `postgres_ping` 和 `redis_ping`。

## 取消与恢复

诊断运行时按一次 Ctrl-C 会取消当前 context，并把已完成和正在执行的调用状态保存到
`.runs/<run_id>.json`。stderr 中输出的 `run_id` 可用于查看状态：

```bash
sre-agent status --run-id <run_id>
```

对于 `cancelled`、`timed_out` 或 `failed` 的 run，可以恢复同一个 run：

```bash
sre-agent resume --run-id <run_id> --task-timeout 2m
```

若 run 仍为 `running`，先确认原进程已经停止，再明确确认恢复：

```bash
sre-agent resume --run-id <run_id> --resume-running
```

未知结果的 `smoke_run` 不允许自动恢复，因为该合成事务可能已经产生写入；详见
[运行恢复说明](run-recovery.md)。

## 依赖连通性检查

```bash
sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario dependency-check \
  --goal "检查 chat_proj 的 PostgreSQL 和 Redis 是否可连接"
```

当前 mock 流程：

1. `postgres_ping` 发送 PostgreSQL startup message，检查服务端是否响应。
2. `redis_ping` 发送 Redis `PING`，期望返回 `PONG`。
3. 报告会基于 PostgreSQL/Redis 工具返回的真实 observation 展示 evidence。

## WebSocket 连接失败

```bash
sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario websocket \
  --goal "诊断 chat_proj WebSocket 为什么连接失败"
```

当前 mock 流程：

1. `websocket_check` 尝试 WebSocket HTTP Upgrade 握手。
2. `log_read` 过滤最近的 WebSocket 相关错误。
3. 报告会基于 WebSocket 握手和日志读取的 trace observation 展示 evidence。
