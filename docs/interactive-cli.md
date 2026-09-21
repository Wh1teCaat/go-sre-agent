# 交互式 CLI

对外可执行文件名为 `sre`。源码目录仍是 `cmd/sre-agent`，请显式构建二进制：

```bash
go build -o ./sre ./cmd/sre-agent
./sre --help
```

将 `./sre` 安装为 `$(go env GOPATH)/bin/sre` 后，可直接使用 `sre`。不带子命令会进入逐行交互会话：

```bash
sre
sre --config configs/local.yaml --environment local
sre --session-id <session_id>
```

无子命令但标准输入不是终端时，程序会提示改用 `sre diagnose`、`resume` 等脚本子命令后退出，不会等待输入。未知子命令同样会返回错误，不会被当作诊断目标。

## 输入与会话

- 普通文本和 `/diagnose <问题>` 创建新的 run，并关联当前会话。
- `/new` 会立即创建一个新的空会话并清除最近 run 引用；第一次诊断会写入该会话，既有 run 和跨会话记忆不会删除。
- `/sessions` 列出已保存会话，`/use <session_id>` 只能切换到当前环境的会话。
- `/runs` 只列出当前会话关联的 run。`/status`、`/report`、`/plan` 和 `/evidence` 默认使用最近一次 run，也可显式传入 run ID。
- `/resume` 不带 ID 时先列出可恢复记录，必须选择其中一个；仍为 `running` 的 run 还需输入 `yes` 确认原进程已停止。

命令参数支持单双引号和反斜杠转义，由本地解析器处理，不通过 shell 执行。未知的 `/` 命令只显示帮助，不会发送给模型。`/llm chat <消息>` 是直接模型调试，不创建诊断 run；普通文本始终是 SRE 诊断。

## 自动记忆与证据边界

诊断开始时会自动：

1. 加载当前会话的 `.sessions/<session_id>/memory.md`。
2. 用服务、环境和诊断目标检索 `memories/` 中最多 3 条、最多 12 KiB 的跨会话资料。
3. 在终端显示实际命中数及来源 run ID。
4. 将历史资料作为待验证线索，而不是当前证据、系统指令或工具授权。
5. 在 run、会话状态保存后，按既有收录规则自动更新跨会话记忆。

没有命中时诊断照常进行。记忆文件损坏、人工修改保护拒绝覆盖或写入失败都会显示错误；程序不会把失败描述成已收录成功。 `/memory search|collect|rebuild|invalidate|correct|delete` 仅用于查看和维护，正常诊断不依赖它们。

## 可用命令

```text
/help [command]
/diagnose <问题>
/resume [run_id]
/status [run_id]
/report [run_id]
/runs
/sessions
/use <session_id>
/new
/plan [run_id]
/evidence [run_id]
/config
/llm ping
/llm chat <消息>
/eval mock [scenario]
/eval model [scenario]
/memory search <关键词>
/memory collect [run_id]
/memory rebuild
/memory invalidate <run_id>
/memory correct <run_id>
/memory delete <run_id>
/exit
```

`/config` 只显示配置路径、服务、环境、模式和目录，不显示连接串、地址或凭据。`/memory delete` 会先展示精确 run ID 和删除原因，只有输入 `yes` 才会逻辑删除；原始 run 和复盘仍保留。`/eval model` 同样需要输入 `yes`，取消时不会发起真实模型请求。

## 进度、取消与退出

工具检查的开始、完成/失败、耗时和摘要会由既有结构化进度事件渲染；诊断报告和结构化状态保持在标准输出，普通进度写入标准错误。所有终端内容会脱敏并清除危险控制字符，不显示模型内部推理。

一次只运行一个任务。运行时按 Ctrl-C 会取消该任务、持久化当前状态并回到提示符；每次任务使用独立 context，下一次诊断可以正常开始。空闲时 Ctrl-C 只取消当前输入。Ctrl-D 或 `/exit` 正常退出；SIGTERM 会请求取消任务并保存状态后退出。恢复会话不会自动恢复未完成任务，请显式使用 `/resume`。

诊断运行完成不等同于根因已经确定，更不等同于服务已经恢复；终端会分别显示运行状态和结论强度。

## 不依赖外部服务的演示

以下演示使用进程内 mock，不访问真实模型、Docker 或诊断目标：

```text
$ sre --config configs/config.yaml --mock-scenario skeleton
SRE Agent
环境：local  模式：mock/skeleton
会话：session_...
输入问题开始诊断，/help 查看命令。

sre > 检查登录接口
历史记忆：没有匹配记录，继续使用当前检查取证。
诊断：...
运行：run_...
报告已保存，输入 /report 查看。

sre > /status
...

sre > /exit
```

用于自动化的非交互入口保持不变，例如 `sre diagnose --goal "排查接口异常"`、`sre resume --run-id <run_id>`、`sre report --run-id <run_id>`、`sre memory ...`、`sre eval ...` 和 `sre llm ...`。
