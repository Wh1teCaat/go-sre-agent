# 阶段 0：评测与回归约定

本文定义阶段 0 已实现的评测入口和结果判定边界。

## 固定 mock 回归

固定 mock 回归用于稳定地检查固定诊断 runtime 和兼容性样本是否发生
回归。旧运行的报告重建由兼容性 fixture 测试单独覆盖。可审查的场景清单
提交到 `evals/scenarios/`；可执行的固定输入、模型响应和 in-process 工具
替身定义在 `internal/eval/`，两者必须同步更新。

它必须不联网：不得读取真实模型凭据、请求模型服务、访问真实目标、
调用 Docker，或产生付费调用。`--mock-scenario` 只固定当前诊断的
模型 action，未必天然隔离工具目标；评测入口应使用独立的固定 fixture，
不能把现有需要本地服务的诊断场景直接当作离线评测。

入口：

```bash
sre eval mock
```

该命令应执行全部固定场景，将每个场景标记为 `passed` 或 `failed`，
并在有失败时以非零状态退出。

## 真实模型评测

真实模型评测是显式的人工操作，不属于 CI、普通回归或默认验收。

```bash
# 默认只记录 skipped，绝不发起模型请求。
sre eval model

# 只有明确授权后才允许读取配置并调用真实模型。
sre eval model --execute-real-model
```

没有 `--execute-real-model` 时，结果必须是 `skipped`，并说明“未授权
执行真实模型”。`skipped` 不是 `passed`。只有实际完成模型调用、保存了
相应记录并满足该评测断言时，才能记录为 `passed`；凭据缺失、网络错误、
超时或断言失败都不能表述为通过。

执行带 flag 的命令前，操作者应自行确认 provider、目标模型、费用和
网络权限。该命令也不应把 API key、请求正文中的敏感信息或原始响应中的
凭据写入结果文件。

阶段 0 的 `login-500` 真实模型评测不会连接操作者配置的业务目标，甚至
不会打开诊断网络连接或读取日志文件：它只向模型展示 `.invalid` 的固定
目标，并由进程内锁定工具返回 HTTP 500 和错误日志 observation。任意其他
URL、方法、日志路径和工具都会被拒绝。`--config` 仅用于读取 agent 的 skill
和超时配置。这让被评测的变量限定为模型决策，而不是生产服务状态。

## 结果记录

每次评测默认在 `evals/results/` 写入一个脱敏结果记录（也可用
`--results-dir` 显式覆盖），至少包含：

- 时间、命令、场景或模型标识，以及场景/fixture 版本；
- `passed`、`failed` 或 `skipped` 状态和原因；
- 是否在显式授权后尝试调用真实 provider（不代表请求一定已到达远端）；
- 产生诊断 trace 的评测的断言摘要和 trace 步数；`skipped` 或 provider
  初始化失败的记录只保存未执行或失败原因。

`evals/results/` 是运行产物，默认忽略，不提交 Git。若使用
`--results-dir`，操作者负责选择同等受保护、不会提交的目录。评测定义、
脱敏 fixture 和断言可以提交；真实环境地址、凭据、原始诊断记录和真实
模型输出不能作为 Git 样本提交。

## 旧运行 v1 fixture

兼容性 fixture 放在受版本控制的脱敏 testdata 目录中，而不是从
`.runs/` 复制真实运行。v1 fixture 的范围是当前无会话字段的运行 JSON：
`run_id`、`goal`、`status`、`plan`、`diagnosis`、`trace`、`error`、
`created_at` 和 `updated_at`。它应刻意省略后续版本可能增加的
`session_id`、调用 checkpoint/call 状态和跨会话记忆字段。

至少应覆盖一个已完成运行的加载、状态查询和报告重建；若恢复仍支持
旧失败运行，则另提供一个失败运行 fixture 并验证其恢复入口。fixture
只证明格式兼容，不证明历史结论在当前环境仍然成立。

## 验收命令

阶段 0 实现完成后，可按以下顺序验收：

```bash
go test ./...
go vet ./...
sre eval mock
sre eval model
```

最后一条默认应显示 `skipped`，不能被报告为真实模型评测通过。仅在
操作者明确决定承担真实调用后，才额外执行：

```bash
sre eval model --execute-real-model
```

真实模型命令没有实际执行时，不应在验收记录中填写通过。
