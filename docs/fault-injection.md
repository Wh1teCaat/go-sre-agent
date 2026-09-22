# Go Chat Compose 故障注入与 Agent 验收

本文用于在本地测试环境复现故障，并验证 Agent 是否能形成完整证据链。所有命令都假设：

- `go-chat` 位于 `/home/y2/project/go-chat`（Compose 服务：edge、backend、frontend、postgres、redis、kafka）。
- Agent 位于 `/home/y2/project/go-sre-agent`。
- 使用 `configs/config.yaml`，其中日志目录和当前 Compose 容器白名单已配置。
- 仅在可丢弃的本地环境执行。默认配置不会启用会创建测试数据的 `smoke_run`。

首次使用时，从随仓库提交的模板创建本地配置（实际运行配置仍保持忽略，不应提交）：

```bash
cd /home/y2/project/go-sre-agent
cp configs/go-chat-compose.example.yaml configs/config.yaml
```

先启动服务并确认基线：

```bash
cd /home/y2/project/go-chat
docker compose up -d --build
docker compose ps
```

当前 Compose 的 backend 有两个副本，名称通常为 `go-chat-backend-1` 和
`go-chat-backend-2`。若 Compose 项目名不同，先执行 `docker compose ps --format json`，
再更新 Agent 配置中的 `docker_compose_project`；`docker_ps` 会按允许的 Compose 服务
发现实际副本。恢复单个服务使用
`docker compose up -d --no-deps <service>`。

## 场景一：全部 backend 副本退出

注入故障：

```bash
cd /home/y2/project/go-chat
docker compose stop backend
```

执行诊断：

```bash
cd /home/y2/project/go-sre-agent
./sre diagnose \
  --config configs/config.yaml \
  --max-steps 10 \
  --goal "诊断 go-chat 登录入口为什么不可用。检查 edge HTTP 响应、两个 backend 副本的容器状态、退出码和最近容器日志，并给出基于本次证据的结论。"
```

验收点：报告应同时引用 `http_check` 的异常响应和 `docker_ps` 或
`docker_inspect` 的 backend 状态；不能把 401/404 误判为进程未存活。nginx
返回 502/504 只能说明该次代理请求失败，仍需结合 backend 容器证据解释。

恢复：

```bash
cd /home/y2/project/go-chat
docker compose up -d --no-deps backend
```

## 场景二：Redis 不可达

注入故障：

```bash
cd /home/y2/project/go-chat
docker compose stop redis
```

执行诊断：

```bash
cd /home/y2/project/go-sre-agent
./sre diagnose \
  --config configs/config.yaml \
  --max-steps 12 \
  --goal "诊断 go-chat 的 Redis 依赖是否可用。检查 Redis 连通性、edge /health 响应体、Redis 容器状态和 backend 最近日志，区分 Redis 当前不可达、数据库健康状态与消息投递状态。最终结论只能引用本次 trace。"
```

验收点：Agent 应把 `redis_ping` 失败保留为 observation，继续检查容器和日志，
而不是在工具失败后直接退出。当前 go-chat 的 `/health` 在 Redis 不可达时仍可能返回
HTTP 200；这不表示 Redis 或跨实例推送健康。

恢复：

```bash
cd /home/y2/project/go-chat
docker compose up -d --no-deps redis
```

如果 backend 已退出，再执行 `docker compose up -d --no-deps backend`。

## 场景三：PostgreSQL 不可达

该场景不修改表结构或业务数据。

注入故障：

```bash
cd /home/y2/project/go-chat
docker compose stop postgres
```

执行诊断：

```bash
cd /home/y2/project/go-sre-agent
./sre diagnose \
  --config configs/config.yaml \
  --max-steps 12 \
  --goal "诊断 go-chat 数据库依赖是否可用。检查 edge /health、PostgreSQL 连接、PostgreSQL 容器状态和 backend 最近日志；区分诊断 DSN 失败与应用实际数据库故障。"
```

验收点：报告应保留 PostgreSQL 连接失败、`/health` 的 503 或实际响应、容器状态等各自
的范围；不得仅凭 Agent 的 DSN 失败就断言应用无法连接。

恢复：

```bash
cd /home/y2/project/go-chat
docker compose up -d --no-deps postgres
```

## 场景四：WebSocket 缺少认证

Go Chat 的真实 WebSocket 路由是 `/v1/ws`，且经过认证中间件。不带凭据进行握手即可得到稳定的认证失败证据，不需要修改服务：

```bash
cd /home/y2/project/go-sre-agent
./sre diagnose \
  --config configs/config.yaml \
  --max-steps 10 \
  --goal "诊断 go-chat WebSocket /v1/ws 为什么连接失败。检查握手状态、backend 状态和最近 WebSocket 相关日志，区分服务不可达、鉴权失败与协议升级失败。"
```

验收点：报告应明确“服务有 HTTP 响应”和“握手成功”不是一回事；401 代表认证失败证据，不应表述为后端进程未存活。

## 完成后的清理检查

```bash
cd /home/y2/project/go-chat
docker compose up -d
docker compose ps
```

每次运行都会生成 `run_id`。可用 `status` 查看计划和 trace，或用 `report` 重建报告：

```bash
./sre status --config configs/config.yaml --run-id <run_id>
./sre report --config configs/config.yaml --run-id <run_id>
```

## 自动化回归

`scripts/fault-injection.sh` 把上述场景脚本化：注入故障 → 运行诊断 → 断言报告包含
对应工具证据和结构化 `## Root Cause` 段落 → 恢复服务。

```bash
scripts/fault-injection.sh              # 全部场景
scripts/fault-injection.sh backend-down # 单个场景
```

`CHAT_DIR` 与 `AGENT_DIR` 环境变量可覆盖默认目录。
