# chat_proj 故障注入与 Agent 验收

本文用于在本地测试环境复现故障，并验证 Agent 是否能形成完整证据链。所有命令都假设：

- `go-chat` 位于 `/home/y2/project/go-chat`（compose 服务：edge/gateway/logic/frontend/postgres/redis）。
- Agent 位于 `/home/y2/project/go-sre-agent`。
- 使用 `configs/config.yaml`，其中日志目录和 Docker 容器白名单已配置。
- 只在可丢弃的本地数据库执行数据库故障场景。

先启动服务并确认基线：

```bash
cd /home/y2/project/go-chat
docker compose up -d
docker compose ps
```

注意：`docker compose start <service>` 会校验依赖（如 kafka）且无法修复失效的
bind mount；恢复单个服务请使用 `docker compose up -d --no-deps <service>`。

## 场景一：后端容器退出

注入故障：

```bash
cd /home/y2/project/chat_proj
docker compose stop backend
```

执行诊断：

```bash
cd /home/y2/project/go-sre-agent
go run ./cmd/sre-agent diagnose \
  --config configs/config.yaml \
  --max-steps 10 \
  --goal "诊断 chat_proj 后端为什么无法访问。检查 HTTP、容器状态、退出码和最近容器日志，并给出基于本次证据的结论。"
```

验收点：报告应同时引用 `http_check` 的连接失败证据和 `docker_ps` 或 `docker_inspect` 的容器状态，不能把 401/404 误判为进程未存活。

恢复：

```bash
cd /home/y2/project/chat_proj
docker compose start backend
```

## 场景二：Redis 不可达

注入故障：

```bash
cd /home/y2/project/chat_proj
docker compose stop redis
```

执行诊断：

```bash
cd /home/y2/project/go-sre-agent
go run ./cmd/sre-agent diagnose \
  --config configs/config.yaml \
  --max-steps 12 \
  --goal "诊断 chat_proj 当前依赖健康状态。检查后端、Redis 连通性、Redis 容器状态和后端最近日志，判断缓存故障是否影响服务启动或登录链路。最终结论只能引用本次 trace。"
```

验收点：Agent 应把 `redis_ping` 失败保留为 observation，继续检查容器和日志，而不是在工具失败后直接退出。

恢复：

```bash
cd /home/y2/project/chat_proj
docker compose start redis
```

如果后端因 Redis 故障已经退出，再执行 `docker compose start backend`。

## 场景三：users 表缺失导致登录异常

此场景会临时修改本地测试库结构。先确认没有其他人使用该数据库。

注入故障：

```bash
cd /home/y2/project/chat_proj
docker compose exec postgres psql -U postgres -d chat_proj \
  -c 'ALTER TABLE users RENAME TO users_fault_injection;'
```

执行诊断：

```bash
cd /home/y2/project/go-sre-agent
go run ./cmd/sre-agent diagnose \
  --config configs/config.yaml \
  --max-steps 12 \
  --goal '诊断 chat_proj 登录链路。使用 POST JSON body {"email":"test@example.com","password":"testpwd"} 请求 /v1/user/login，检查 PostgreSQL 认证和 users 表、后端容器状态及最近 ERROR 日志；若没有复现 500，必须报告本次实际状态。'
```

验收点：`postgres_check` 应报告 `users` 表不存在；登录请求的实际状态码、数据库检查和日志证据应彼此区分，模型不能仅凭历史记忆断言根因。

恢复：

```bash
cd /home/y2/project/chat_proj
docker compose exec postgres psql -U postgres -d chat_proj \
  -c 'ALTER TABLE users_fault_injection RENAME TO users;'
```

## 场景四：WebSocket 缺少认证

`chat_proj` 的真实 WebSocket 路由是 `/v1/ws`，且经过认证中间件。不带凭据进行握手即可得到稳定的认证失败证据，不需要修改服务：

```bash
cd /home/y2/project/go-sre-agent
go run ./cmd/sre-agent diagnose \
  --config configs/config.yaml \
  --max-steps 10 \
  --goal "诊断 chat_proj WebSocket /v1/ws 为什么连接失败。检查握手状态、后端状态和最近 websocket 相关日志，区分服务不可达、鉴权失败与协议升级失败。"
```

验收点：报告应明确“服务有 HTTP 响应”和“握手成功”不是一回事；401 代表认证失败证据，不应表述为后端进程未存活。

## 完成后的清理检查

```bash
cd /home/y2/project/chat_proj
docker compose start postgres redis backend frontend
docker compose ps
```

每次运行都会生成 `run_id`。可用 `status` 查看计划和 trace，或用 `report` 重建报告：

```bash
go run ./cmd/sre-agent status --config configs/config.yaml --run-id <run_id>
go run ./cmd/sre-agent report --config configs/config.yaml --run-id <run_id>
```

## 自动化回归

`scripts/fault-injection.sh` 把上述场景脚本化：注入故障 → 运行诊断 → 断言报告包含
对应工具证据和结构化 `## Root Cause` 段落 → 恢复服务。

```bash
scripts/fault-injection.sh              # 全部场景
scripts/fault-injection.sh backend-down # 单个场景
```

`CHAT_DIR` 与 `AGENT_DIR` 环境变量可覆盖默认目录。
