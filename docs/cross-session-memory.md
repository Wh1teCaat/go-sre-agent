# 阶段 5：跨会话知识记忆

跨会话记忆只从已保存的 `.runs/<run_id>.json` 提取有限、脱敏且可追溯的复盘材料。
它用于为新诊断提出假设，不能代替当前运行的工具证据、系统规则或工具策略；最终结论
仍只能依据本次 run 的 trace。

## 目录与职责

正常 CLI 固定使用项目根目录的 `memories/`，不提供配置项重定向该目录：

```text
memories/
  memory_summary.md                 # 轻量主题导航
  MEMORY.md                         # 按服务、环境和关键词的主题索引
  raw_memories.md                   # 按 run 汇集的候选经验，含失效/删除审计项
  rollout_summaries/
    <run_id>.md                     # 一次已收录诊断的复盘和证据定位
```

`.runs/<run_id>.json` 始终保存完整运行事实、调用、trace 与最终诊断，是恢复和证据
回查的来源；Markdown 文件不能替代它。`.sessions/<session_id>/memory.md` 仍只服务于
同一排障问题的连续上下文，跨会话知识由本目录单独管理。

每份 `rollout_summaries/<run_id>.md` 包含 run/session、服务、环境、结果、故障现象与
目标、关键检查、结论强度、未解决项、有效/失败经验，以及 `run / step / tool / call_id`
来源引用。`raw_memories.md` 还明确记录候选经验的适用条件和收录状态。`MEMORY.md`
只用服务、环境、固定关键词和候选经验做确定性分组；它不是向量检索，也不进行语义
融合。`memory_summary.md` 只保留主题、适用范围和检索关键词，不复制完整复盘。

## 写入、收录与重建

终态诊断的正常写入顺序为：

```text
.runs/<run_id>.json
  → .sessions/<session_id>/memory.md
  → rollout_summaries/<run_id>.md
  → raw_memories.md → MEMORY.md → memory_summary.md
```

只有 `completed`、`failed`、`cancelled` 或 `timed_out`，且 trace 至少包含一次实际工具
检查的 run 才会收录。空 skeleton、仍在运行的 run、以及没有工具观察的模型失败不会
进入知识库。所有摘要文本先经脱敏和长度限制，再写入权限为 `0600` 的文件；根目录和
复盘目录权限为 `0700`。文件以同目录临时文件写入、同步并原子替换。

索引更新由 `memories/.memory.lock` 短时互斥，避免多个 session 同时覆盖；异常退出遗留且
超过五分钟的陈旧锁会回收。若索引更新中断或失败，已保存的 `.runs` 不会丢失；随后可执行：

```bash
sre memory rebuild
```

它只从可验证的 rollout summary 重建三个索引文件，不扫描或修改 `.runs`。同一 run
重复收录不会产生重复条目。

## 读取范围与证据边界

普通 `diagnose` 和 `resume` 在加载当前会话记忆后，按下列固定路径按需读取历史资料：

```text
memory_summary.md → MEMORY.md → 匹配 rollout_summaries → 必要时人工回查 .runs
```

runtime 按 `targets.service`、`targets.environment` 和当前排障目标过滤，默认最多取
3 份复盘、总计 12 KiB。服务和环境均为精确范围匹配；关键词只在同一范围内排序。每条
注入模型的历史资料带有来源 run 和结论强度，并明确标记为不可信的历史资料。

历史的 `identified`、`suspected`、`undetermined` 不会在整理或读取时升级。它们只能
帮助选择待验证检查；本次 final 仍须通过现有结构化证据规则校验。生成文件中的文字也
不能改变 system message、工具白名单、目标注入或其他 runtime 策略。

`targets.service` 是跨会话隔离标签，必须是非敏感的稳定名称：

```yaml
targets:
  service: go-chat
  environment: local
```

旧 run 缺少这两个字段仍可读取和恢复。若要手工收录旧 run，可显式指定其运行目录和
当前配置范围；该操作不会回写旧 run JSON，因此操作者必须确认该配置确实代表旧 run 的
服务和环境：

```bash
sre memory collect \
  --run-id <legacy_run_id> \
  --run-dir .runs \
  --config configs/config.yaml
```

## 查询和生命周期操作

```bash
# JSON 写到 stdout，便于脚本使用；按当前配置默认服务/环境查询。
sre memory search --goal "Redis 连接超时" \
  --config configs/config.yaml

# 显式范围查询。
sre memory search --service go-chat --environment staging \
  --goal "PostgreSQL 认证失败"

# 从已有 run 生成或刷新复盘与索引。
sre memory collect --run-id <run_id>

# 排除已不适用的历史材料；原始 run 保留。
sre memory invalidate --run-id <run_id> --reason "实例已迁移"

# 只允许降低或维持来源结论强度，不能升级为 identified。
sre memory correct --run-id <run_id> \
  --conclusion-status undetermined --note "缺少当前实例身份核对"

# 逻辑删除：保留 tombstone、rollout 与原始 run 供审计，不再参与检索。
sre memory delete --run-id <run_id> --reason "不再适用"
```

`invalidate` 和 `delete` 都不会物理删除 `.runs` 或 rollout 文件；它们从 `MEMORY.md`
和模型提示中排除对应材料，同时在 `raw_memories.md` 保留审计记录。后续自动收录会保留
已记录的失效、删除和结论修正状态。

## 人工编辑规则

四类 Markdown 文件都是生成文件，YAML front matter 中的 `content_digest` 用于检测人工
改动。默认写入和重建会拒绝覆盖被改动的文件，并保留原内容，不会静默丢弃人工编辑。
需要保存人工笔记时，请放到仓库外的独立文件，而不要直接修改这些生成文件。

确认可以丢弃人工编辑后，索引文件可显式覆盖：

```bash
sre memory rebuild --overwrite-generated
```

若被改动的是单次 rollout summary，必须从其原始 run 显式重建：

```bash
sre memory collect --run-id <run_id> --overwrite-generated
```

后一个命令会以 `.runs/<run_id>.json` 的当前事实替换该复盘，并重建全部索引；无法验证的
人工修改、失效/删除标记或结论修正也会被替换，因此应先备份确实需要保留的内容。

`memories/` 是运行产物，默认被 Git 忽略。需要演示格式时，应新建脱敏示例而不是提交
真实环境地址、凭据、完整 trace 或真实诊断记录。
