# 示例

## 真实 LLM 连通性测试

如果只想确认 `.env` 里的模型配置能否调用成功，可以直接测试底层 `ChatClient`：

```bash
sre llm ping
```

也可以输入自己的消息：

```bash
sre llm chat --message "用一句话说明 SRE 诊断 Agent 是什么"
```

这条路径只打印模型原始文本，不进入 Agent runtime，也不会调用工具。

## 真实 LLM 诊断烟测

```bash
sre diagnose \
  --config configs/config.example.yaml \
  --goal "仅验证真实 ActionPlanner 和 ChatClient 接入，不调用工具，直接生成 final 诊断"
```

当 `.env` 设置了真实 provider 时，这条路径会使用 `ActionPlanner` 和对应 ChatClient。想跑本地固定链路时，加上 `--mock-scenario skeleton`。

## 登录接口 500

```bash
sre diagnose \
  --config configs/config.example.yaml \
  --mock-scenario login-500 \
  --goal "诊断 go-chat 登录接口 /v1/user/login 为什么返回 500"
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
sre status --run-id <run_id>
```

对于 `cancelled`、`timed_out` 或 `failed` 的 run，可以恢复同一个 run：

```bash
sre resume --run-id <run_id> --task-timeout 2m
```

若 run 仍为 `running`，先确认原进程已经停止，再明确确认恢复：

```bash
sre resume --run-id <run_id> --resume-running
```

未知结果的 `smoke_run` 不允许自动恢复，因为该合成事务可能已经产生写入；详见
[运行恢复说明](run-recovery.md)。

## 依赖连通性检查

```bash
sre diagnose \
  --config configs/config.example.yaml \
  --mock-scenario dependency-check \
  --goal "检查 go-chat 的 PostgreSQL 和 Redis 是否可连接"
```

当前 mock 流程：

1. `postgres_ping` 发送 PostgreSQL startup message，检查服务端是否响应。
2. `redis_ping` 发送 Redis `PING`，期望返回 `PONG`。
3. 报告会基于 PostgreSQL/Redis 工具返回的真实 observation 展示 evidence。

## WebSocket 连接失败

```bash
sre diagnose \
  --config configs/config.example.yaml \
  --mock-scenario websocket \
  --goal "诊断 go-chat WebSocket 为什么连接失败"
```

当前 mock 流程：

1. `websocket_check` 尝试 WebSocket HTTP Upgrade 握手。
2. `log_read` 过滤最近的 WebSocket 相关错误。
3. 报告会基于 WebSocket 握手和日志读取的 trace observation 展示 evidence。

## 使用真实测试账号检查登录

`http_check` 支持为固定登录地址自动注入测试账号：

```yaml
targets:
  allowed_post_urls:
    - http://localhost:8080/v1/user/login
  test_account:
    url: http://localhost:8080/v1/user/login
    credentials_file: configs/login-account.secret
```

凭据文件是包含 `email`、`password` 的 JSON，建议权限 0600，使用被 Git
忽略的 `.secret` 后缀。也可用 `email_env` 和 `password_env` 指定进程环境变量名，
与文件方式互斥；不读取 LLM 的 `.env`。URL 必须同时满足主机和 POST 白名单。

使用真实模型运行 `sre diagnose --goal "用测试账号验证登录，并关联日志排查异常"`，
不要加 `--mock-scenario`。模型仍调用 `http_check`，参数只需登录 URL 和
`method: POST`，不提供 body；工具内部读取账号并发送 JSON 邮箱和密码。
注入仅匹配配置的精确 URL 和 POST，其他地址与 GET/HEAD 不携带测试凭据。

响应保留 HTTP 状态、耗时、请求的 request ID、业务码与 `session_issued`
（是否返回非空令牌），不返回登录正文、账号密码或令牌。模型可以关联 request ID
查日志，再检查依赖；测试账号成功不能排除其他用户的问题。缺少凭据时明确报错，
不会退化为空 POST。凭据请求拒绝自定义 body 和 repeat>1，不跟随重定向。
启用测试账号后，`http_check` 保守按可能产生会话的工具处理，不能在并行批次中调用，
也不会自动恢复结果未知的调用。
