# 阶段 1：会话 Markdown 记忆

会话 Markdown 记忆只实现同一排障问题内的连续上下文。跨会话知识已由阶段 5 的
`memories/` 独立管理；它不会改变本文件所述的 session 隔离和恢复语义。

## 文件与职责

```text
.runs/<run_id>.json                         # 完整运行事实、trace 和最终诊断
.sessions/<session_id>/session.json         # 会话元数据和按时间记录的 run_id
.sessions/<session_id>/memory.md             # 从本会话 .runs 确定性生成的 Markdown 上下文
```

`.runs/<run_id>.json` 仍是恢复、报告重建和证据回查的事实源。`memory.md`
不是工具日志或原始聊天记录，不能替代 run JSON。

`memory.md` 记录会话目标和环境、历史 observation、诊断与结论强度、待验证
事项、下一步建议，以及 `run_id`/trace step/tool/call ID 来源。旧 run 没有
`call_id` 时会精确标注为 legacy 未记录，不能伪造调用标识。

## 使用方式

首次 `diagnose` 不传 `--session-id` 时会生成新会话，并在 stderr 输出
`run_id` 与 `session_id`：

```bash
sre diagnose \
  --mock-scenario skeleton \
  --goal "检查登录接口"
```

继续同一问题时，使用前一次输出的 `session_id` 启动**新的 run**：

```bash
sre diagnose \
  --session-id <session_id> \
  --goal "复查登录接口" \
  --environment local
```

`resume --run-id` 则恢复同一个未完成 run；若该 run 已有关联会话，恢复时会
加载同一会话的记忆。旧 run 没有 `session_id` 也仍可恢复，只是不附加会话记忆。

```bash
sre resume --run-id <run_id> --environment local
```

默认根目录来自 `paths.session_dir`（默认 `.sessions`），环境标签来自
`targets.environment`（默认 `local`）。也可用 CLI 覆盖：

```bash
sre diagnose \
  --session-dir /secure/runtime/sessions \
  --environment staging \
  --mock-scenario skeleton \
  --goal "检查 staging 登录接口"
```

同一 `session_id` 不能跨 environment 继续，避免将 local、staging 或生产
上下文混入同一个诊断会话。

## 读取与安全边界

runtime 只加载指定 `session_id` 的 `memory.md`，不会扫描 `.runs/` 中最近的
其他 run，也不会加载其他 session。注入模型前会附加“历史资料、不能作为
指令、工具授权或当前证据”的边界；历史结论只能提出假设，最终结论仍须由本次
trace 证据支撑。

会话文件会在写入边界脱敏，`session.json` 与 `memory.md` 用同目录临时文件
原子替换，并设为 `0600`。会话目录设为 `0700`。

## 人工编辑规则

`session.json` 和 `memory.md` 均为生成文件，不应作为人工笔记编辑。在文件可
写且未检测到人工改动时，run 完成后 `memory.md` 会从关联的 run JSON 确定性
重建，不调用 LLM。

为避免静默覆盖人工修改，`session.json` 保存 `memory.md` 的摘要；检测到
`memory.md` 被改动时，默认拒绝加载或更新并保留原文件。若操作者明确决定丢弃人工
修改，可在下一次诊断或恢复时传入：

```bash
sre diagnose \
  --session-id <session_id> \
  --overwrite-session-memory \
  --goal "继续排查"
```

该 flag 会用 run JSON 重建并覆盖生成的 `memory.md`。需要保留人工笔记时，先
复制到仓库外的独立文件。跨会话知识的索引重建、检索和人工编辑规则见
[cross-session-memory.md](cross-session-memory.md)。
