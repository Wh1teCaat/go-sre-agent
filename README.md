# go-sre-agent

`go-sre-agent` 是一个用 Go 编写的只读 SRE 诊断 Agent。用户提供排障目标后，LLM 会选择诊断工具、分析执行结果，并生成带有证据链的 Markdown 报告。

项目不依赖 LangChain、AutoGen 等 Agent 框架，核心 runtime、工具调用、策略校验和 trace 均由 Go 实现。

## 核心能力

- 诊断 HTTP、日志、PostgreSQL、Redis、WebSocket 和 Docker 容器。
- 支持 OpenAI-compatible、Ollama、Anthropic 以及本地 mock 场景。
- 默认使用 ReAct；复杂任务由 LLM 按需请求 plan，plan 不占用执行 step。
- 保存完整 trace 和运行状态，支持查询、恢复和重新生成报告。
- 在每次 LLM/工具调用前后原子 checkpoint，支持 Ctrl-C、任务总超时和受限恢复。
- 对模型观测应用总量与单条输出预算，原始 trace 不受裁剪影响并保留回查引用。
- 支持带独立 `call_id` 的有限并行只读检查，以及 stderr 实时进度输出。
- 为同一个排障问题保存隔离的会话 Markdown 记忆，连续诊断只加载指定会话。
- 将符合条件的已完成 run 确定性整理为按服务和环境隔离的跨会话知识；历史资料仅用于提出待验证假设。
- 通过工具白名单、目标白名单、参数校验、超时和脱敏限制执行边界。
- 最终报告只能引用本次运行中真实存在的工具证据。
- 每条工具证据会保留检查执行状态、目标健康状态、目标身份、时间和脱敏后的结构化事实。

## 工作流程

```text
诊断目标
   ↓
LLM 决定直接行动或按需生成计划
   ↓
Policy 校验 action
   ↓
执行只读工具并记录 observation / trace
   ↓
继续决策，直到生成诊断报告
```

详细设计见 [架构文档](docs/architecture.md)；运行取消和恢复规则见
[运行恢复说明](docs/run-recovery.md)。

## 快速开始

### 环境要求

- Go 1.22+
- 可选：OpenAI-compatible、Ollama 或 Anthropic 模型服务
- 可选：Docker CLI（仅使用 Docker 诊断工具时需要）

### 1. 构建或安装 `sre`

`cmd/sre-agent` 是源码目录名；为避免 `go install` 默认生成 `sre-agent`，
请显式指定对外二进制名：

```bash
go build -o ./sre ./cmd/sre-agent
# 可选：安装到 Go 的 bin 目录，之后可在 PATH 中直接使用 sre
install -m 0755 ./sre "$(go env GOPATH)/bin/sre"
```

以下示例假设 `sre` 已在 `PATH`；仅做本地构建时将其替换为 `./sre`。

### 2. 准备配置

```bash
cp configs/config.example.yaml configs/config.yaml
cp .env.example .env
```

在 `configs/config.yaml` 中设置被诊断程序的地址、依赖连接信息和访问白名单；未传 `--config` 时 CLI 会自动读取该文件。使用其他路径时再显式传入 `--config`。如需使用真实模型，在 `.env` 中填写对应 provider 的地址、模型和 api。

### 3. 验证模型连接

```bash
sre llm ping
```

也可以跳过真实模型，直接运行内置 mock 场景：

```bash
sre diagnose \
  --mock-scenario login-500 \
  --goal "诊断登录接口为什么返回 500"
```

### 4. 运行真实诊断

```bash
sre diagnose \
  --goal "诊断登录接口为什么返回 500"
```

报告会输出到终端；配置了 `paths.report_dir` 后也会保存为 Markdown 文件。运行状态默认保存在 `.runs/`，同一问题的会话状态默认保存在 `.sessions/`。

### 5. 进入交互会话

stdin、stdout 都连接可用终端时，`sre` 默认进入全屏 TUI；`--plain` 保留原有逐行交互。普通文本会创建诊断 run；斜杠命令由本地解析器处理，不会交给模型或 shell。

```bash
sre --config configs/config.yaml --environment local
sre --plain  # 原逐行模式
# 恢复指定会话的已保存视图（不会自动恢复未完成 run）
sre --session-id <session_id>
```

每次诊断会自动加载当前会话历史，并按服务、环境和目标查询至多 3 条、总计 12 KiB 的跨会话记忆作为待验证线索；无需先执行 `/memory`。交互命令、取消和恢复说明见 [交互式 CLI](docs/interactive-cli.md)。完全离线的 TUI 演示可运行 `sre --config configs/tui-demo.yaml --mock-scenario tui-demo`；演示工具只返回进程内模拟结果。

## 配置

主要配置项位于 `configs/config.yaml`：

```yaml
agent:
  max_steps: 12
  llm_timeout: 30s
  tool_timeout: 5s
  task_timeout: 5m
  max_tool_calls: 24
  max_parallel_tools: 2
  context_budget_bytes: 49152
  tool_output_budget_bytes: 8192
  skill_path: skills/sre-diagnosis/SKILL.md

policy:
  tool_allowlist:
    - http_check
    - log_read
  allowed_log_dirs:
    - ./testdata/logs
  allowed_hosts:
    - localhost

paths:
  run_dir: .runs
  session_dir: .sessions
  report_dir: reports

targets:
  # 跨会话知识隔离标签，必须使用非敏感的稳定服务名。
  service: go-chat
  environment: local
  backend_base_url: http://localhost:8080
  # http_check 允许 POST 复现的诊断地址；未配置时回退为 backend_base_url 派生的登录地址。
  allowed_post_urls:
    - http://localhost:8080/v1/user/login
  postgres_dsn: postgres://postgres:postgres@localhost:5432/app?sslmode=disable
  redis_addr: localhost:6379
  websocket_url: ws://localhost:8080/ws
  log_file: ./testdata/logs/app.log
```

完整的通用示例见 [configs/config.example.yaml](configs/config.example.yaml)。对本机 Go Chat
Compose 使用 [configs/go-chat-compose.example.yaml](configs/go-chat-compose.example.yaml) 创建
本地 `configs/config.yaml`；LLM 环境变量见 [.env.example](.env.example)。

诊断规则位于仓库内的 [skills/sre-diagnosis/SKILL.md](skills/sre-diagnosis/SKILL.md)，不再写在 Go 源码中。`agent.skill_path` 支持绝对路径或相对于当前工作目录的路径。使用真实 LLM 启动诊断时，程序只读取一次该文件、去掉 YAML frontmatter，并把正文作为 system message 发送给模型；文件不存在、为空或超过 128 KiB 会阻止启动。mock 场景不调用模型，因此不需要加载 skill。

支持的 provider：

| Provider | `SRE_AGENT_LLM_PROVIDER` | 配置前缀 |
| --- | --- | --- |
| OpenAI-compatible | `openai_compatible` | `MIMO_*` 或 `OPENAI_*` |
| Ollama | `ollama` | `OLLAMA_*` |
| Anthropic | `anthropic` | `ANTHROPIC_*` |

## 常用命令

```bash
# 测试模型连接
sre llm ping

# 发送一条模型消息
sre llm chat --message "hello"

# 查看运行状态
sre status --run-id <run_id>

# 恢复未完成的运行
sre resume --run-id <run_id> --max-steps 20

# 使用已有会话开始新的连续诊断 run
sre diagnose --session-id <session_id> --goal "复查当前问题"

# 根据已保存的 trace 重新生成报告
sre report --run-id <run_id>

# 查询固定 memories/ 根目录中的同服务、同环境历史复盘（JSON 输出）
sre memory search --goal "Redis 连接超时"

# 从已保存 run 手动收录或重建跨会话索引
sre memory collect --run-id <run_id>
sre memory rebuild
```

会话记忆的文件结构、隔离规则和人工编辑处理见 [docs/session-memory.md](docs/session-memory.md)。
跨会话知识的收录条件、检索预算、生命周期和人工编辑规则见
[docs/cross-session-memory.md](docs/cross-session-memory.md)。
上下文预算、并行工具调用和 CLI 输出流规则见
[docs/runtime-context-progress.md](docs/runtime-context-progress.md)。

可用 mock 场景：

- `skeleton`：验证 CLI 与报告链路。
- `login-500`：执行 HTTP 与日志诊断。
- `dependency-check`：检查 PostgreSQL 与 Redis。
- `websocket`：执行 WebSocket 与日志诊断。

## 诊断工具

| 工具 | 用途 |
| --- | --- |
| `http_check` | 检查 HTTP 状态、延迟和响应片段；仅保存配置白名单中的响应头，`repeat` 可汇总状态和响应头分布 |
| `log_read` | 读取允许目录中的近期日志，可按关键词、精确 `request_id` 或时间窗口（`since`/`last_minutes`）过滤 |
| `postgres_ping` | 检查 PostgreSQL 协议层可达性 |
| `postgres_check` | 检查 PostgreSQL 认证、SQL 连通性和表是否存在 |
| `redis_ping` | 通过 `PING/PONG` 检查 Redis |
| `redis_check` | 读取 Redis INFO：内存用量、驱逐、连接数、keyspace 规模和实例身份指纹 |
| `redis_scan` | 在前缀白名单内做键级只读查询（SCAN + TTL），核对 presence/token 等服务端状态 |
| `kafka_check` | 只读 Kafka 健康检查：broker 探活、cluster id、topic 分区数、消费组与活跃 lag |
| `kafka_compose_check` | 在配置的 Kafka 容器内执行固定只读检查，读取 topic 分区、消费组和 lag；适用于 broker 未映射宿主机端口的 Compose 环境 |
| `websocket_check` | 检查 WebSocket HTTP Upgrade 握手；`ping` 可验证协议层消息通路 |
| `docker_ps` | 查看允许容器的运行状态；可按配置的 Compose 项目和服务发现动态副本名称 |
| `docker_inspect` | 查看允许容器的状态、退出码和健康状态 |
| `docker_logs` | 读取允许容器的近期日志，可按最近分钟数和关键词过滤 |
| `docker_stats` | 查看允许容器的 CPU、内存、PID 用量和重启次数 |
| `docker_probe` | 在允许容器内执行固定只读探测模板：未发布端口的 /health、nginx 上游快照、容器内 PostgreSQL 身份 |
| `smoke_run` | 合成事务：执行配置的端到端冒烟脚本定位消息链路故障环节（唯一非只读工具，需显式配置启用） |

## 安全边界

- 所有诊断工具默认只读，并经过工具和参数 schema 校验。
- HTTP、WebSocket、日志目录和 Docker 容器受配置白名单限制；响应头只按显式白名单保存。
- PostgreSQL DSN 和 Redis 地址由 runtime 注入，模型不能指定其他目标。
- HTTP 默认仅允许 `GET`/`HEAD`；写请求受严格限制。
- 工具和 LLM 请求均有独立超时。
- `tool_calls` 只允许不同工具的独立只读检查；每项都有独立 checkpoint 和 `call_id`，并受调用总预算与并发上限约束。
- 模型上下文中的大工具输出会被裁剪并标识 `context_truncated`；完整脱敏证据仍在 trace/run 中。
- 常见 password、token、API key 和 secret 会在 trace 中脱敏。
- 模型生成的 evidence 必须匹配本次真实 trace。
- 根因结论必须以结构化 `root_cause` 声明（`identified`/`suspected`/`undetermined`）；判定为 `identified` 时必须绑定真实 trace 证据。
- `identified` 还需满足故障类型的必要证据规则；`suspected` 必须明确支持证据和待验证事项。字段说明与示例见[结构化证据与根因约束](docs/evidence-constraints.md)。
- `docker_probe` 与 `kafka_compose_check` 只能执行固定的只读命令；`docker_ps` 发现的
  动态容器也必须属于配置的 Compose 项目和服务，`redis_scan` 受键前缀白名单约束。
- `postgres_check`/`redis_check` 会输出实例身份指纹（版本、平台、run_id），用于识别端口被无关实例占据的冒名场景。
- `smoke_run` 是唯一的非只读工具（测试账号真实写入），默认不存在，仅在配置显式写出 `targets.smoke_command` 时注册，且命令内容模型不可指定。

本项目提供执行边界，但接入真实环境前仍应使用最小权限凭据，并检查配置中的目标和白名单。

## 测试

```bash
go test ./...
go vet ./...
```

真实故障注入与验收步骤见 [docs/fault-injection.md](docs/fault-injection.md)。
固定回归和显式真实模型评测的边界见 [docs/evaluations.md](docs/evaluations.md)。

## 项目结构

```text
cmd/sre-agent/       CLI 入口
configs/             示例配置
docs/                架构、示例和故障注入文档
internal/agent/      Agent runtime
internal/llm/        Planner 与模型客户端
internal/policy/     action 和工具调用校验
internal/tools/      诊断工具及注册表
internal/trace/      trace 记录
internal/run/        运行状态持久化
internal/memory/     跨会话复盘、索引、检索与生命周期管理
internal/report/     Markdown 报告生成
```

## 当前限制

- `websocket_check` 的 `ping` 仅验证协议层 ping/pong，不收发业务消息。
- `postgres_ping` 仅检查协议层可达性；需要认证和 SQL 证据时应使用 `postgres_check`。
- 动态诊断建议主要由模型生成。
- 跨会话检索仅使用服务、环境和固定关键词；尚未引入向量数据库或语义知识融合。

## 路线图

- 运行历史增长后，将 `.runs` 迁移到 SQLite 并增加相关性检索。
- 持续补充真实故障注入回归样本（见 `scripts/fault-injection.sh`）。

更多示例见 [docs/examples.md](docs/examples.md)。
