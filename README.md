# go-sre-agent

`go-sre-agent` 是一个用 Go 编写的 SRE 诊断 Agent 项目。它不使用 LangChain、AutoGen 等 Agent 框架，也不是传统 CRUD 后端或聊天机器人。

这个项目的目标是实现一个“自动任务执行器”：用户输入排障目标后，系统通过 LLM 决定下一步调用哪个只读诊断工具，执行工具，记录结果，再继续判断，最后生成一份基于证据的诊断报告。

## 当前状态

当前版本已经是可运行的 CLI MVP：HTTP、日志、PostgreSQL、Redis、WebSocket 和 Docker 容器诊断均有真实只读实现。CLI 可以通过 `ActionPlanner + ChatClient` 调用 OpenAI-compatible、Ollama 或 Anthropic，也可以用 mock 场景执行固定工具链路，并生成带 trace 的 markdown 报告。

已经完成：

- Go module 初始化。
- CLI 入口。
- `Tool` interface 和工具注册表。
- `LLM Provider` interface。
- mock LLM provider。
- 通用 `ChatClient` / `Message` / `ChatRequest` / `ChatResponse` 抽象。
- `ActionPlanner`：负责把诊断上下文组装成通用 chat 请求，并把模型输出解析成结构化 action。
- `ActionPlanner` 支持从模型返回的 JSON code fence 中提取 action，解析失败时返回短 content preview，便于排查真实模型输出。
- provider-neutral LLM 配置和 `ChatClient`：OpenAI-compatible/Ollama 调用 `/v1/chat/completions`，Anthropic 调用 `/v1/messages`。
- LLM 连通性 CLI：`llm ping` / `llm chat` 可以直接查看真实模型原始返回。
- `.env` / `.env.example` 支持 MIMO/OpenAI-compatible、Ollama 和 Anthropic 配置。
- Agent runtime loop。
- policy validator。
- memory trace store。
- markdown report 生成器。
- `http_check`：真实 HTTP 请求，返回 status、latency、body snippet。
- `log_read`：读取允许目录下的日志文件，支持最近 N 行和单个/多个关键词过滤。
- `log_read` 使用有界环形缓冲读取大日志，内存占用由最大返回行数限制，并在扫描中响应工具超时。
- `redis_ping`：使用 Redis RESP 协议发送 `PING`，验证是否返回 `PONG`。
- `postgres_ping`：发送 PostgreSQL startup message，验证 PostgreSQL 协议层是否可达。
- `postgres_check`：使用 PostgreSQL SQL driver 完成认证、SQL ping，并可检查表是否存在。
- `websocket_check`：发送 WebSocket HTTP Upgrade 握手，返回 101 成功或失败状态。
- `docker_ps`、`docker_inspect`、`docker_logs`：通过容器白名单读取状态、退出码、健康状态和最近日志，不开放任意 Docker 命令。
- `login-500` mock 场景：执行 `http_check -> log_read -> final`。
- `dependency-check` mock 场景：执行 `postgres_ping -> redis_ping -> final`。
- `websocket` mock 场景：执行 `websocket_check -> log_read -> final`。
- YAML 配置解析：仓库提供 `configs/config.example.yaml`，本地目标配置使用被忽略的 `configs/config.yaml`。
- Policy 会基于工具 schema 校验必填参数和基础 JSON 类型。
- Policy 在工具 schema 已知时会拒绝未知参数，避免 LLM 传入未声明 args。
- `http_check` 和 `websocket_check` 已接入 allowed host 限制。
- `http_check` 默认只允许 GET/HEAD；POST 仅放行配置中的登录诊断 URL，PUT/PATCH/DELETE 会在发请求前拒绝。
- PostgreSQL DSN 和 Redis 地址由 runtime 强制注入配置目标，模型不能改成其他依赖主机。
- 工具执行失败不会直接终止 runtime；失败会记录为带 `error` 字段的 observation，并在下一轮发回 LLM 继续判断。
- 每次模型决策有独立超时；模型请求、action 校验或 final 证据校验失败时，同一步最多进行一次带错误反馈的纠错重试。
- Trace 同时记录 plan、tool_call、final 的 action 类型、模型名、模型耗时和尝试次数。
- Runtime 会在 final 返回前校验 evidence 的 step/tool 必须对应真实 trace。
- 诊断运行会保存到 `.runs/<run_id>.json`，可以用 `status` 和 `report` 子命令回查。
- 新诊断会读取最近 3 个已完成运行的摘要作为历史线索；历史线索不能作为本次报告证据。
- `http_check` body snippet 和 `log_read` 日志行会脱敏常见 password/token/api_key/secret。
- runtime、registry、policy、trace 的基础单元测试。
- `http_check`、`log_read`、`redis_ping`、`postgres_ping`、`postgres_check`、`websocket_check` 和 CLI mock 场景测试。
- 配置示例、架构文档、示例诊断文档。

当前限制：

- `postgres_ping` 仍保留为协议层可达性检查；生产诊断优先使用 `postgres_check` 获取认证和 SQL 层证据。
- `websocket_check` 只做握手诊断，不做 WebSocket 业务消息收发。
- final diagnosis 的动态建议还主要依赖模型输出，后续可以继续增加基于 trace 的建议模板兜底。

## 目录结构

```text
cmd/sre-agent/              CLI 入口
configs/                    示例配置
docs/                       架构和示例文档
internal/agent/             Agent runtime loop、状态和执行边界
internal/config/            配置结构
internal/llm/               LLM provider interface、ActionPlanner、ChatClient、配置和各协议 client
internal/policy/            安全策略和 action 校验
internal/report/            Markdown 诊断报告生成
internal/schema/            Action、Observation、Diagnosis 等结构化类型
internal/tools/             Tool interface、registry 和工具包
internal/trace/             每一步执行 trace 的记录和存储
internal/run/               运行状态持久化与恢复
examples/chat_proj/         面向 chat_proj 的示例诊断流程
testdata/logs/              测试和示例日志
```

## 设计目标

这个项目重点不在“封装一个 LLM API”，而在这些能力：

- 不依赖 Agent 框架，自己实现 runtime loop。
- 使用结构化 action 调用工具，不靠自然语言随便解析。
- 工具调用经过 policy 校验。
- 每一步都有 trace：step、thought summary、tool name、args、result、error、duration。
- 工具默认只读，并受白名单、超时、路径和 host 限制约束。
- 最终诊断报告必须基于工具证据，证据不足时不能编造原因。
- LLM 分两层抽象：`Provider.NextAction` 面向 Agent runtime，`ChatClient.Chat` 面向不同模型 API，方便后续替换模型。

## CLI 使用方式

如果只是想确认 `.env` 里的真实模型配置是否可用，可以先跑 LLM 连通性测试。这个命令只调用底层 `ChatClient`，直接打印模型原始返回，不进入 Agent runtime，也不会调用诊断工具：

```bash
go run ./cmd/sre-agent llm ping
```

也可以自己输入一条消息查看返回：

```bash
go run ./cmd/sre-agent llm chat --message "用一句话说明 SRE 诊断 Agent 是什么"
```

如果 `.env` 中配置了真实 LLM provider 和对应凭据，不传 `--mock-scenario` 时 CLI 会调用真实模型：

```bash
go run ./cmd/sre-agent diagnose \
  --goal "诊断 chat_proj 登录接口 /v1/user/login 为什么返回 500"
```

如果想强制跑本地 mock，可以显式传 `--mock-scenario skeleton`：

```bash
go run ./cmd/sre-agent diagnose \
  --mock-scenario skeleton \
  --goal "验证 CLI 和报告输出"
```

输出到指定文件：

```bash
go run ./cmd/sre-agent diagnose \
  --goal "诊断 chat_proj WebSocket 为什么连接失败" \
  --out report.md
```

每次 `diagnose` 会在 `paths.run_dir` 指定的目录下保存一份运行状态，未配置时默认使用 `.runs/`。成功或失败时 stderr 都会打印 `run_id`，方便后续查询或恢复：

```bash
go run ./cmd/sre-agent status \
  --config configs/config.example.yaml \
  --run-id run_20260706_120000_000000000_ab12cd34ef56ab78
```

`status` 的 JSON 会同时显示当前 plan、各检查项状态和已记录的 trace 数量，便于判断失败运行应从哪里继续。

如果某次诊断失败或达到步数上限，可以用同一个 `run_id` 继续。`resume` 会加载旧 trace，从下一步继续规划，并更新同一个 run state 文件。`--max-steps` 必须大于已有 trace 的最大 step：

```bash
go run ./cmd/sre-agent resume \
  --config configs/config.example.yaml \
  --run-id run_20260706_120000_000000000_ab12cd34ef56ab78 \
  --max-steps 6
```

也可以根据已保存的 trace 和 diagnosis 重新生成报告：

```bash
go run ./cmd/sre-agent report \
  --config configs/config.example.yaml \
  --run-id run_20260706_120000_000000000_ab12cd34ef56ab78
```

路径类默认值放在配置文件的 `paths` 段里：

```yaml
paths:
  run_dir: .runs
  report_dir: reports
```

配置了 `paths.report_dir` 时，`diagnose`/`resume` 不传 `--out` 也会把报告写到 `<report_dir>/<run_id>.md`；同时仍会在 CLI 打印报告，方便直接查看。`--out` 可覆盖为单次指定文件。

当前已经可以跑 `login-500` mock 场景。这个场景会请求登录接口，并读取错误日志：

```bash
go run ./cmd/sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario login-500 \
  --goal "诊断 chat_proj 登录接口 /v1/user/login 为什么返回 500"
```

目标地址、PostgreSQL、Redis、WebSocket、日志路径和 allowed hosts 都放在 YAML 配置里。CLI 只保留 goal、mock 场景和运行控制参数：

```bash
go run ./cmd/sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario websocket \
  --goal "诊断 chat_proj WebSocket 为什么连接失败"
```

也可以跑依赖连通性检查：

```bash
go run ./cmd/sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario dependency-check \
  --goal "检查 chat_proj 的 PostgreSQL 和 Redis 是否可连接"
```

也可以跑 WebSocket 握手诊断：

```bash
go run ./cmd/sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario websocket \
  --goal "诊断 chat_proj WebSocket 为什么连接失败"
```

## Agent 执行流程

1. CLI 接收用户目标和配置。
2. Runtime 初始化 LLM provider、tool registry、policy validator 和 trace store。
3. Runtime 从 trace store 推导历史 observations，真实 LLM 模式下由 `ActionPlanner` 把目标、工具 schema 和 observations 组装成通用 `ChatRequest`。
4. 配置的 `ChatClient` 把通用请求翻译成 OpenAI-compatible `/v1/chat/completions` 或 Anthropic `/v1/messages` 请求。
5. 模型返回 JSON content 后，`ActionPlanner` 解析成结构化 action，例如：

```json
{
  "type": "tool_call",
  "thought_summary": "先检查后端健康状态",
  "tool": "http_check",
  "args": {
    "url": "http://localhost:8080/health"
  }
}
```

6. Policy 校验 action 是否允许执行。
7. Executor 调用对应工具并记录 observation。
8. 如果工具执行失败，Runtime 会把错误写入 trace 和 observation，再进入下一轮规划。
9. Trace store 记录 action 类型、模型决策耗时和尝试次数；工具 action 还记录工具、参数、结果、错误和执行耗时。
10. 循环直到 LLM 返回 `final`，或达到 `max_steps`。恢复运行时，step 会从已有 trace 的最大 step 继续递增。
11. CLI 将本次 run state 保存到 `.runs/<run_id>.json`。
12. Report 模块根据完整 trace 和 diagnosis 生成 markdown 报告。

## 安全边界

当前已经实现的安全限制包括：

- 最大执行步数。
- 工具白名单。
- 工具参数基础 schema 校验。
- 未声明工具参数拒绝执行。
- final action 必须包含非空诊断 summary。
- 每个工具调用的超时控制。
- 每个 LLM 决策的超时控制和单次纠错重试。
- 只读诊断工具，不执行生产环境写操作。
- 工具失败会作为证据继续交给 LLM，不会丢失错误上下文。
- `log_read` 只能读取配置允许目录下的日志文件。
- `http_check` 和 `log_read` 会对常见敏感字段做脱敏后再写入 observation。
- HTTP 和 WebSocket 工具只能访问允许的 host。
- Docker 工具只能读取明确配置的容器，日志最多读取 500 行且会脱敏。
- 报告只能引用已有 observation 和 trace 作为证据。
- final diagnosis 的 evidence 必须引用本次真实 trace 中存在的 step/tool，否则 runtime 返回错误。

示例配置的 `max_steps` 为 12。plan、replan、工具调用和 final 都计入 step；简单任务可以通过配置或 `--max-steps` 调低，复杂任务达到上限后可用 `resume` 继续。

## 面向 chat_proj 的目标场景

当前 MVP 覆盖这些诊断问题：

- 后端服务是否存活。
- PostgreSQL 是否可连接。
- Redis 是否可连接。
- 登录接口 `/v1/user/login` 为什么返回 500。
- WebSocket 为什么连接失败。
- 最近日志里是否有关键错误。
- 根据工具结果生成诊断结论、证据和建议。

## 测试

运行测试：

```bash
go test ./...
go vet ./...
```

当前已有测试覆盖：

- Agent runtime loop。
- 工具失败后继续下一轮规划，并把失败 observation 传回 LLM。
- Tool registry。
- Policy action 校验、工具参数 schema 校验和未知参数拒绝。
- final action 的非空 summary 校验。
- Trace memory store。
- `http_check`：使用 `httptest` 验证 HTTP 状态、耗时、body snippet、敏感字段脱敏、方法限制、重定向和 allowed host 限制。
- `log_read`：使用临时日志文件验证最近 N 行、关键词过滤、敏感字段脱敏和路径限制。
- `redis_ping`：使用 fake Redis TCP server 验证 RESP `PING/PONG`。
- `postgres_ping`：使用 fake PostgreSQL TCP server 验证 startup message 和服务端响应解析。
- `postgres_check`：使用 fake SQL runner 验证 SQL ping、表存在检查和缺表报告。
- `websocket_check`：使用 fake WebSocket TCP server 验证 101 握手成功、HTTP 500 握手失败、参数校验和 allowed host 限制。
- Docker 工具：使用 fake command runner 验证容器白名单、状态解析、日志上限和脱敏，不依赖本机 Docker daemon。
- YAML config 加载、duration 解析和运行参数覆盖规则。
- Markdown report 只展示 trace-backed evidence，并过滤没有对应 trace 的模型证据声明。
- 通用 `ActionPlanner` 的 `ChatRequest` 构造、action JSON 解析、JSON code fence 提取和解析错误预览。
- provider 配置选择、OpenAI-compatible/Ollama Chat Completions 与 Anthropic Messages 请求和响应解析。
- CLI `login-500`、`dependency-check` 和 `websocket` mock 场景。
- CLI 在未指定 `--mock-scenario` 时切换到 `ActionPlanner + 配置的 ChatClient`。

面向真实 `chat_proj` 的可恢复故障注入与验收步骤见 [docs/fault-injection.md](docs/fault-injection.md)。仓库的 GitHub Actions 会在 push 和 pull request 时运行单元测试与 `go vet`。

## LLM 环境变量

项目已提供 `.env.example`，本地 `.env` 已配置默认模型：

```bash
SRE_AGENT_LLM_PROVIDER=openai_compatible
MIMO_API_KEY=
MIMO_BASE_URL=
MIMO_MODEL=gpt-4o-mini
```

`MIMO_*` 使用 OpenAI-compatible 协议。仍可使用 `OPENAI_API_KEY` / `OPENAI_BASE_URL` / `OPENAI_MODEL`，但 MIMO 配置优先。`.env` 已加入 `.gitignore`，避免误提交密钥。

Ollama 直接复用 OpenAI-compatible client：

```bash
SRE_AGENT_LLM_PROVIDER=ollama
OLLAMA_BASE_URL=http://localhost:11434/v1
OLLAMA_MODEL=qwen3:8b
```

Anthropic 使用独立的 Messages API client：

```bash
SRE_AGENT_LLM_PROVIDER=anthropic
ANTHROPIC_API_KEY=
ANTHROPIC_BASE_URL=https://api.anthropic.com/v1
ANTHROPIC_MODEL=你的 Claude 模型 ID
```

当前代码已经会读取这些配置。CLI 规则是：

- `llm ping` / `llm chat`：只测试底层 `ChatClient`，直接输出模型文本。
- 传了 `--mock-scenario`：强制使用 mock provider，适合测试固定链路。
- 没传 `--mock-scenario`，且 provider 为 `openai_compatible`、`ollama` 或 `anthropic`：使用真实 `ActionPlanner + 对应 ChatClient`。
- 没传 `--mock-scenario`，且 provider 为 `mock` 或未配置：使用 `skeleton` mock。

## 简历卖点

这个项目可以突出：

- 用 Go 从零实现 Agent runtime，而不是调用现成 Agent 框架。
- 结构化工具调用和安全策略设计。
- 面向 SRE 场景的证据链、trace 和诊断报告。
- Plan/execute/replan 与 ReAct 工具循环结合，工具失败可回传模型继续诊断。
- LLM 分层解耦，支持 mock provider、通用 ActionPlanner，以及 OpenAI-compatible、Ollama、Anthropic ChatClient。
- 只读工具、路径/主机/容器限制、双层超时、敏感信息脱敏和结构化参数校验。
- 可持久化 run state、resume、证据引用校验和可复现故障注入场景。

## 下一步

MVP 路线已经完成。后续增强项不影响当前诊断闭环：

1. 让 `websocket_check` 支持可选业务消息收发，用于排查握手成功但业务连接失败的问题。
2. 运行历史明显增大后，把 `.runs` 扫描式记忆迁移到 SQLite，并增加相关性检索。
3. 在独立测试环境持续运行 chat_proj 故障注入场景，积累真实回归样本。
