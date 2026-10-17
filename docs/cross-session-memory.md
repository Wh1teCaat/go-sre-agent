# 跨会话知识记忆

跨会话记忆只从已保存的 `.runs/<run_id>.json` 提取有限、脱敏且可追溯的复盘材料。
它用于为新诊断提出假设，不能代替当前运行的工具证据、系统规则或工具策略；最终结论
仍只能依据本次 run 的 trace。

## 目录与职责

正常 CLI 固定使用项目根目录的 `memories/`，不提供配置项重定向该目录：

```text
memories/
  memory_summary.md                 # 模型检索入口：主题、何时查阅、检索词
  MEMORY.md                         # 模型按主题整理的详细经验库
  raw_memories.md                   # 逐 Run 原始经验，作为模型整合材料，含审计项
  rollout_summaries/
    <run_id>.md                     # 一次已收录诊断的复盘和证据定位
  extractions/
    <run_id>.json                   # 模型提取的候选经验和来源引用
  consolidations/
    <scope_hash>.json               # 同一服务与环境的整合结果
```

`.runs/<run_id>.json` 始终保存完整运行事实、调用、trace 与最终诊断，是恢复和证据
回查的来源；Markdown 文件不能替代它。`.sessions/<session_id>/memory.md` 仍只服务于
同一排障问题的连续上下文，跨会话知识由本目录单独管理。

每份 `rollout_summaries/<run_id>.md` 包含 run/session、服务、环境、结果、故障现象与
目标、关键检查、结论强度、未解决项、有效/失败经验，以及 `run / step / tool / call_id`
来源引用。保存时先生成规则复盘作为回退；模型提取完成后，详细模型复盘会写入同一
`rollout_summaries/<run_id>.md`，其原始经验、待验证项与引用写入 `extractions/`，并完整
投影到 `raw_memories.md`。模型按同服务、同环境的原始经验材料整合，生成详细知识、
适用条件、冲突、自由检索词和独立导航摘要。它们分别进入 `MEMORY.md` 和 `memory_summary.md`。

三个文件分别服务于不同阶段：

| 文件 | 职责 | 消费者 |
| --- | --- | --- |
| `raw_memories.md` | 逐 Run 原始经验，包括细节、反证、未解决项与来源 | 整合模型；人工审计 |
| `MEMORY.md` | 跨 Run 整理后的经验正文、适用条件、冲突与来源 | 检索模型选择主题后按需读取 |
| `memory_summary.md` | 简短主题提示、何时查阅、检索词和来源 ID，不复制经验正文 | 检索模型的第一阶段 |

整合请求使用与 `raw_memories.md` 相同的原始经验投影，由有效提取结果构造，仅包含当前
服务和环境；不会把全库原始经验塞入新诊断。JSON 仍是带来源摘要的校验存储格式，
Markdown 由程序渲染；内容选择、主题归纳和相关性判断交给模型。无需向量数据库。

## 写入、收录与重建

终态诊断的正常写入顺序为：

```text
.runs/<run_id>.json
  → .sessions/<session_id>/memory.md
  → rollout_summaries/<run_id>.md
  → raw_memories.md → MEMORY.md → memory_summary.md
  → [启用模型记忆时] 分块提取完整观察和诊断
  → 更新 rollout_summaries/<run_id>.md + extractions/<run_id>.json
  → raw_memories.md（逐 Run 原始经验）
  → 同范围模型整合 → consolidations/<scope_hash>.json
  → MEMORY.md（详细经验）+ memory_summary.md（轻量导航）
```

只有 `completed`、`failed`、`cancelled` 或 `timed_out`，且 trace 至少包含一次实际工具
检查的 run 才会收录。空 skeleton、仍在运行的 run、以及没有工具观察的模型失败不会
进入知识库。这只是来源资格判断；模型可以进一步判定该 Run 没有可复用经验，保存跳过原因，
不进入主题索引且不反复提取。所有模型文本先脱敏、校验容量，再写入权限为 `0600` 的文件；根目录和
复盘目录权限为 `0700`。文件以同目录临时文件写入、同步并原子替换。

索引更新由 `memories/.memory.lock` 短时互斥，避免多个 session 同时覆盖；异常退出遗留且
超过五分钟的陈旧锁会回收。若索引更新中断或失败，已保存的 `.runs` 不会丢失；随后可执行：

```bash
sre memory rebuild
```

它离线读取并验证已有的复盘、提取和整合文件，再重建三个索引文件；不会调用模型。
模型文件过期或无效时只索引确定性复盘。同一 run 重复收录不会产生重复条目。

## 可选模型提取与整合

默认 `memory.model_enabled: false`。启用后，一次诊断先保存 run、session 和确定性复盘；
模型失败或超时不改变诊断结果。交互界面在后台逐项处理，并在退出时取消模型请求。
启动、保存新 run 和下一次启动都会从文件状态重新发现待办。一次性 `diagnose` 则在保存后
按配置的时限处理一轮。

```yaml
memory:
  model_enabled: true
  extract_model: ""        # 留空时使用当前 provider 的默认模型
  consolidation_model: ""  # 也用于诊断前的导航/经验选择，可与 extract_model 相同
  timeout: 30s
  round_limit: 2
```

也可以显式处理待办，`--dry-run` 不调用模型：

```bash
sre memory process --config configs/config.yaml --limit 4 --timeout 30s
sre memory process --run-id <run_id> --dry-run
sre memory rebuild --config configs/config.yaml --run-dir .runs
```

每个 JSON 文件含版本、内容摘要和来源摘要，使用 `0600` 权限原子写入。

提取输入包含目标、完整结构化诊断（含反证与待验证项）、计划，以及每条工具观察的
事实、数据、目标身份、状态、时间、参数和错误；递归脱敏后按大小分块，不再只取前
16 条观察或每条 300 字节摘要。模型可以保留多段 Markdown、具体命令、错误特征和
适用条件，也可以返回空经验及跳过原因。任一分块失败时不提交部分提取；一次 Run 的
全部分块共享 `memory.timeout`。超大单条观察（128 KiB）、请求（256 KiB）或输出超限
会使该项处理失败，保留来源与回退材料，不静默截掉尾部当作完整经验。

每条候选最多 8 KiB，单 Run 最多 128 条，模型复盘合计最多 128 KiB；同范围整合最多
128 个有效提取、64 个主题，每个主题正文最多 16 KiB。容量是处理边界，不强迫模型凑
条数，也不限制检索词必须来自固定词表。过大的整合输入不会被静默截成部分 Run。

引用必须匹配保存的 `run_id / step / tool / call_id`，且分块提取只能引用该分块实际提供
的观察。整合保留适用条件、冲突和来源 Run；输入摘要变化会使整合结果失效。写入前
再次检查来源，模型调用期间不持有索引锁；短期租约减少多进程重复调用。结论强度由
来源和人工修正状态决定；提示词要求自由文本不得升级结论，但引用校验并不等于语义
正确性证明。历史内容仍需本次证据验证。

旧版 `model-memory-v1` JSON 保留校验读取能力，但不再视为有效模型经验。
`memory process` 或后台 worker 会从保存的 Run 重新提取为 v2；`memory rebuild` 不调用
模型，因此只提供规则回退。有效新产物的重复收录和离线重建保持幂等。

## 读取范围与证据边界

普通 `diagnose` 和 `resume` 在加载当前会话记忆后，启用模型记忆时按以下路径读取：

```text
按服务、环境过滤有效 memory_summary.md 导航
    ↓ 模型根据当前问题的含义选主题（可返回无匹配）
读取 MEMORY.md 中被选中的经验正文
    ↓ 模型选来源 Run，并判断是否需要更详细的复盘
只在需要时展开对应模型复盘和来源引用
    ↓ 校验来源未变化，组成历史提示
本次诊断模型
```

这是诊断开始前最多两次检索模型请求，不是给诊断模型开放任意文件访问。
两次请求共享 `memory.timeout`，也受整次任务超时约束。第一阶段只暴露轻量导航，
第二阶段只暴露选中的主题；最多选 8 个主题，最后默认最多返回 3 个来源 Run、总计
12 KiB。服务和环境在模型选择之前隔离，选择结果只能引用所提供的主题和 Run。
模型返回无匹配时不硬塞历史；调用失败或非法选择时回退本地检索。

模型关闭或使用 mock 时，仍可离线按范围和关键词检索规则复盘及有效经验；自由模型
关键词也参与排序。`sre memory search` 和 TUI `/memory` 查询保持离线，用于检查索引，
不等同于诊断时的语义选择。每条注入资料带来源 Run、结论强度和历史假设声明。

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
