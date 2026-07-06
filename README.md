# go-sre-agent

`go-sre-agent` 是一个用 Go 编写的 SRE 诊断 Agent 项目。它不使用 LangChain、AutoGen 等 Agent 框架，也不是传统 CRUD 后端或聊天机器人。

这个项目的目标是实现一个“自动任务执行器”：用户输入排障目标后，系统通过 LLM 决定下一步调用哪个只读诊断工具，执行工具，记录结果，再继续判断，最后生成一份基于证据的诊断报告。

## 当前状态

当前版本已经从纯骨架推进到“可执行的早期 MVP 雏形”：`http_check`、`log_read`、`redis_ping`、`postgres_ping`、`websocket_check` 已有真实实现，CLI 可以通过 `ActionPlanner + OpenAI-compatible ChatClient` 调用真实 LLM，也可以用 mock 场景执行固定工具链路，并生成带 trace 的 markdown 报告。

已经完成：

- Go module 初始化。
- CLI 入口。
- `Tool` interface 和工具注册表。
- `LLM Provider` interface。
- mock LLM provider。
- 通用 `ChatClient` / `Message` / `ChatRequest` / `ChatResponse` 抽象。
- `ActionPlanner`：负责把诊断上下文组装成通用 chat 请求，并把模型输出解析成结构化 action。
- `ActionPlanner` 支持从模型返回的 JSON code fence 中提取 action，解析失败时返回短 content preview，便于排查真实模型输出。
- OpenAI-compatible `ChatClient`：读取 `.env`，调用 `/v1/chat/completions`，返回模型原始 content。
- LLM 连通性 CLI：`llm ping` / `llm chat` 可以直接查看真实模型原始返回。
- `.env` / `.env.example` OpenAI-compatible 配置，默认模型为 `gpt-4o-mini`。
- Agent runtime loop 骨架。
- policy validator。
- memory trace store。
- markdown report 生成器。
- `http_check`：真实 HTTP 请求，返回 status、latency、body snippet。
- `log_read`：读取允许目录下的日志文件，支持最近 N 行和关键词过滤。
- `redis_ping`：使用 Redis RESP 协议发送 `PING`，验证是否返回 `PONG`。
- `postgres_ping`：发送 PostgreSQL startup message，验证 PostgreSQL 协议层是否可达。
- `websocket_check`：发送 WebSocket HTTP Upgrade 握手，返回 101 成功或失败状态。
- Docker inspection 的工具包边界。
- `login-500` mock 场景：执行 `http_check -> log_read -> final`。
- `dependency-check` mock 场景：执行 `postgres_ping -> redis_ping -> final`。
- `websocket` mock 场景：执行 `websocket_check -> log_read -> final`。
- YAML 配置解析：`--config configs/config.example.yaml` 已可用。
- Policy 会基于工具 schema 校验必填参数和基础 JSON 类型。
- Policy 在工具 schema 已知时会拒绝未知参数，避免 LLM 传入未声明 args。
- `http_check` 和 `websocket_check` 已接入 allowed host 限制。
- 工具执行失败不会直接终止 runtime；失败会记录为带 `error` 字段的 observation，并在下一轮发回 LLM 继续判断。
- Runtime 会在 final 返回前校验 evidence 的 step/tool 必须对应真实 trace。
- `http_check` body snippet 和 `log_read` 日志行会脱敏常见 password/token/api_key/secret。
- runtime、registry、policy、trace 的基础单元测试。
- `http_check`、`log_read`、`redis_ping`、`postgres_ping`、`websocket_check` 和 CLI mock 场景测试。
- 配置示例、架构文档、示例诊断文档。

尚未完成：

- `postgres_ping` 还不是完整 SQL 查询；当前只做到协议层 startup-message 可达性检查，后续可接 `database/sql` 或 `pgx` 做认证后的 SQL ping。
- `websocket_check` 只做握手诊断，不做 WebSocket 业务消息收发。
- final diagnosis 的动态建议还主要依赖模型输出，后续可以继续增加基于 trace 的建议模板兜底。

## 目录结构

```text
cmd/sre-agent/              CLI 入口
configs/                    示例配置
docs/                       架构和示例文档
internal/agent/             Agent runtime loop、状态和执行边界
internal/config/            配置结构
internal/llm/               LLM provider interface、ActionPlanner、ChatClient、mock 和 OpenAI-compatible client
internal/policy/            安全策略和 action 校验
internal/report/            Markdown 诊断报告生成
internal/schema/            Action、Observation、Diagnosis 等结构化类型
internal/tools/             Tool interface、registry 和工具包
internal/trace/             每一步执行 trace 的记录和存储
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

如果 `.env` 中配置了 `SRE_AGENT_LLM_PROVIDER=openai_compatible` 和 `OPENAI_API_KEY`，不传 `--mock-scenario` 时 CLI 会调用真实 LLM：

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

输出到文件：

```bash
go run ./cmd/sre-agent diagnose \
  --goal "诊断 chat_proj WebSocket 为什么连接失败" \
  --out report.md
```

当前已经可以跑 `login-500` mock 场景。这个场景会请求登录接口，并读取错误日志：

```bash
go run ./cmd/sre-agent diagnose \
  --mock-scenario login-500 \
  --backend-url http://localhost:8080 \
  --log-file testdata/logs/chat_proj_error.log \
  --allowed-log-dir testdata/logs \
  --goal "诊断 chat_proj 登录接口 /v1/user/login 为什么返回 500"
```

也可以通过 YAML 配置提供默认目标和策略。CLI 显式传入的 flag 会覆盖配置文件中的值：

```bash
go run ./cmd/sre-agent diagnose \
  --config configs/config.example.yaml \
  --mock-scenario websocket \
  --goal "诊断 chat_proj WebSocket 为什么连接失败"
```

也可以跑依赖连通性检查：

```bash
go run ./cmd/sre-agent diagnose \
  --mock-scenario dependency-check \
  --postgres-dsn "postgres://postgres:postgres@localhost:5432/chat_proj?sslmode=disable" \
  --redis-addr localhost:6379 \
  --goal "检查 chat_proj 的 PostgreSQL 和 Redis 是否可连接"
```

也可以跑 WebSocket 握手诊断：

```bash
go run ./cmd/sre-agent diagnose \
  --mock-scenario websocket \
  --websocket-url ws://localhost:8080/ws \
  --log-file testdata/logs/chat_proj_error.log \
  --allowed-log-dir testdata/logs \
  --goal "诊断 chat_proj WebSocket 为什么连接失败"
```

## Agent 执行流程

1. CLI 接收用户目标和配置。
2. Runtime 初始化 LLM provider、tool registry、policy validator 和 trace store。
3. Runtime 从 trace store 推导历史 observations，真实 LLM 模式下由 `ActionPlanner` 把目标、工具 schema 和 observations 组装成通用 `ChatRequest`。
4. `OpenAICompatibleChatClient` 把通用请求翻译成 `/v1/chat/completions` HTTP 请求。
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
9. Trace store 记录本轮 step、工具、参数、结果、错误和耗时。
10. 循环直到 LLM 返回 `final`，或达到 `max_steps`。
11. Report 模块根据完整 trace 和 diagnosis 生成 markdown 报告。

## 安全边界

当前已经实现的安全限制包括：

- 最大执行步数。
- 工具白名单。
- 工具参数基础 schema 校验。
- 未声明工具参数拒绝执行。
- final action 必须包含非空诊断 summary。
- 每个工具调用的超时控制。
- 只读诊断工具，不执行生产环境写操作。
- 工具失败会作为证据继续交给 LLM，不会丢失错误上下文。
- `log_read` 只能读取配置允许目录下的日志文件。
- `http_check` 和 `log_read` 会对常见敏感字段做脱敏后再写入 observation。
- HTTP 和 WebSocket 工具只能访问允许的 host。
- 报告只能引用已有 observation 和 trace 作为证据。
- final diagnosis 的 evidence 必须引用本次真实 trace 中存在的 step/tool，否则 runtime 返回错误。

## 面向 chat_proj 的目标场景

MVP 完成后，希望覆盖这些诊断问题：

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
GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go-mod go test ./...
```

当前已有测试覆盖：

- Agent runtime loop。
- 工具失败后继续下一轮规划，并把失败 observation 传回 LLM。
- Tool registry。
- Policy action 校验、工具参数 schema 校验和未知参数拒绝。
- final action 的非空 summary 校验。
- Trace memory store。
- `http_check`：使用 `httptest` 验证 HTTP 状态、耗时、body snippet、敏感字段脱敏和 allowed host 限制。
- `log_read`：使用临时日志文件验证最近 N 行、关键词过滤、敏感字段脱敏和路径限制。
- `redis_ping`：使用 fake Redis TCP server 验证 RESP `PING/PONG`。
- `postgres_ping`：使用 fake PostgreSQL TCP server 验证 startup message 和服务端响应解析。
- `websocket_check`：使用 fake WebSocket TCP server 验证 101 握手成功、HTTP 500 握手失败、参数校验和 allowed host 限制。
- YAML config 加载、duration 解析和 CLI 覆盖配置规则。
- Markdown report 只展示 trace-backed evidence，并过滤没有对应 trace 的模型证据声明。
- 通用 `ActionPlanner` 的 `ChatRequest` 构造、action JSON 解析、JSON code fence 提取和解析错误预览。
- OpenAI-compatible `ChatClient` 的 `.env` 配置读取、默认模型、Chat Completions 请求和响应 content 解析。
- CLI `login-500`、`dependency-check` 和 `websocket` mock 场景。
- CLI 在未指定 `--mock-scenario` 时切换到 `ActionPlanner + OpenAI-compatible ChatClient`。

## LLM 环境变量

项目已提供 `.env.example`，本地 `.env` 已配置默认模型：

```bash
SRE_AGENT_LLM_PROVIDER=openai_compatible
OPENAI_API_KEY=
OPENAI_BASE_URL=https://api.openai.com/v1
OPENAI_MODEL=gpt-4o-mini
```

`OPENAI_API_KEY` 需要你自己填入真实 key。`.env` 已加入 `.gitignore`，避免误提交密钥。

当前代码已经会读取这些配置。CLI 规则是：

- `llm ping` / `llm chat`：只测试底层 `ChatClient`，直接输出模型文本。
- 传了 `--mock-scenario`：强制使用 mock provider，适合测试固定链路。
- 没传 `--mock-scenario`，且 `SRE_AGENT_LLM_PROVIDER=openai_compatible`：使用真实 `ActionPlanner + OpenAI-compatible ChatClient`。
- 没传 `--mock-scenario`，且 provider 为 `mock` 或未配置：使用 `skeleton` mock。

## 简历卖点

这个项目可以突出：

- 用 Go 从零实现 Agent runtime，而不是调用现成 Agent 框架。
- 结构化工具调用和安全策略设计。
- 面向 SRE 场景的证据链、trace 和诊断报告。
- LLM 分层解耦，支持 mock provider、通用 ActionPlanner 和可替换的 OpenAI-compatible ChatClient。
- 只读工具、路径限制、超时和白名单等生产安全意识。

## 下一步

建议下一阶段继续加强真实诊断质量：

1. 让 final diagnosis 更严格地从 trace observation 动态生成证据。
2. 将 `postgres_ping` 升级为认证后的 SQL ping。
3. 让 `websocket_check` 支持可选业务消息收发，用于排查握手成功但业务连接失败的问题。
