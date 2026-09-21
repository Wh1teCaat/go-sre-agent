# 固定评测场景

本目录保存可提交、脱敏的阶段 0 评测场景说明；实际运行结果默认写入被
Git 忽略的 `evals/results/`，不能放在这里。使用 `--results-dir` 覆盖时，
操作者必须选择同等受保护、不会提交的目录。

`sre-agent eval mock` 运行 `internal/eval.BuiltinScenarios` 中的固定
in-process fixture。它不读取真实模型配置，不访问网络、Docker 或真实服务。
场景的可执行定义与断言保留在 `internal/eval/`，是为了让工具替身、模型
action 和断言在同一次编译中保持一致；本文件是供 review 使用的稳定清单，
修改任一侧时必须同步更新另一侧。

## `skeleton`

- 目标：验证无需工具调用的诊断闭环。
- 固定模型行为：直接返回 final。
- 断言：完成状态、零个工具 observation、摘要包含“闭环”。

## `login-500`

- 目标：验证 HTTP 500 与应用错误日志的证据链。
- 固定模型行为：依次调用 `http_check`、`log_read`，再返回 final。
- 工具替身：分别返回固定的 HTTP 500 和脱敏 `ERROR` 日志匹配 observation。
- 断言：工具顺序、摘要包含“登录接口返回 500”。

这些 fixture 不等同于 `diagnose --mock-scenario`：后者仍会按配置调用实际
诊断工具，适合演示而不是离线基线。真实模型评测的使用边界见
[`docs/evaluations.md`](../../docs/evaluations.md)。
