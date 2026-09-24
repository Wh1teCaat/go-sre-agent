# 交互式 CLI

构建：

```bash
go build -o ./sre ./cmd/sre-agent
```

stdin 和 stdout 都是可用终端时，`sre` 默认打开全屏单列 TUI。`--plain` 使用原有逐行交互。无子命令且 stdin 被重定向时显示用法并退出；`TERM=dumb` 等不支持全屏界面的终端请使用 `--plain`。Linux、macOS、SSH 和 tmux 是第一版的优先验证环境。所有非交互子命令及其输出格式、退出码保持不变。

```bash
sre
sre --session-id <session_id>
sre --config configs/config.yaml
sre --plain
sre diagnose --goal "排查接口异常"
```

## TUI 操作

| 按键 | 行为 |
| --- | --- |
| 普通文本 + Enter | 开始诊断；任务运行中只提示，不排队 |
| `/`、Tab | 显示并补全命令 |
| Alt-Enter / Ctrl-J | 插入换行 |
| 多行粘贴 | 保留为一个草稿，不自动提交；去掉粘贴文本末尾的换行 |
| ↑ / ↓ | 单行草稿浏览历史；多行草稿移动光标；选择器中移动选中项并自动滚动 |
| PgUp / PgDn | 滚动对话；离开底部后暂停自动跟随 |
| Ctrl-O | 打开或关闭工具详情 |
| Esc | 关闭详情、候选、选择器或确认框 |
| q | 关闭报告、证据详情等只读视图 |
| Ctrl-C | 任务中请求取消并等待保存；空闲时清空草稿 |
| Ctrl-D | 空闲且草稿为空时退出 |
| `/exit` | 空闲时退出；运行中取消并保存后退出 |

输入框会随文字折行或显式换行自动扩展，最多显示六行；空间不足时优先保留对话区和底部状态栏。清空或提交后收回为一行。当前终端输入库无法可靠区分 Ctrl-Enter 与 Enter；手动换行请使用 Ctrl-J 或 Alt-Enter。

TUI 根据已保存的模型调用开始/结束事件循环显示 `Thinking.`、`Thinking..`、`Thinking...`，根据真实工具事件用三点方形轮转标记显示 `Calling <tool>...` 及检查结果。当前阶段同时显示在底部状态栏，长检查列表或向上查看历史时仍可见；动画和耗时定时刷新，结构化模型结果会在完成后一次性显示。主回复先显示本次检查支持的健康状态与主要异常，完整证据和报告由命令查看；历史记忆与本次检查仍分别标记，不显示内部推理。界面固定提示使用英文，模型内容和原始证据保留原文。后台工具和命令不会直接写终端。运行和会话历史从已保存的 Run 与 Trace 重建；报告视图从持久化结果重新生成。未保存草稿和逐字符动画不恢复。

`/help`、`/sessions`、`/runs` 和 `/evidence` 使用可搜索的选择列表；`/report`、`/status`、`/plan`、`/config` 打开可滚动的详情。`/resume` 选择失败或取消的 Run 后继续，仍标记为 `running` 的 Run 须先确认原进程已停止。`/memory delete` 与 `/eval model` 使用默认选中取消的确认框。取消确认只关闭当前操作。

常用命令：

```text
/help [command]       /sessions             /use <session_id>
/runs                 /resume [run_id]      /new
/status [run_id]      /plan [run_id]        /evidence [run_id]
/report [run_id]      /config               /diagnose <问题>
/memory search|collect|rebuild|invalidate|correct|delete ...
/eval mock|model ...  /llm ping|chat ...    /exit
```

诊断开始时，Runtime 完成记忆加载后通过同一事件报告实际注入的历史条数和来源 Run。没有命中时继续检查。历史资料只用于提出待验证假设，不能当作本次检查证据。诊断结束后同步保存确定性复盘；主回复不显示收录状态，退出会话时也不生成记忆总结。`/new` 会创建新会话，跨会话记忆仍按服务与环境自动检索。`/config` 只显示脱敏的配置摘要。

设置 `memory.model_enabled: true` 并重启 TUI 后，后台 worker 会在启动时扫描待办、在每个已保存 Run 后继续处理模型提取与整合。完成时，TUI 状态栏显示成功、跳过、过期和失败数量；该通知可能对应启动时发现的旧 Run。退出时会取消模型请求，重启后从文件状态接续。模型记忆只作为历史线索，不进入本次工具证据。显式处理可在终端运行 `sre memory process --run-id <run_id> --limit 2`；此命令是脚本子命令，不是 TUI 的 `/memory` 子命令。

## 完全离线演示

```bash
sre --config configs/tui-demo.yaml --mock-scenario tui-demo
```

输入“登录接口返回 500”即可看到 HTTP、日志与 PostgreSQL 三项模拟检查。演示工具只在进程内返回固定观测，不连接模型、数据库、Docker 或真实服务；Run、会话和报告写入 `.demo-runs/`、`.demo-sessions/`、`.demo-reports/`。退出后终端会恢复原有输入模式和屏幕。`--mock-scenario skeleton` 仍可用于不执行工具的最小闭环。
