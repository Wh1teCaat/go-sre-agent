# 结构化证据与根因约束

阶段 3 为每一次工具观测补充可验证的结构化字段，并将最终结论绑定到本次运行的
trace。它不会执行阶段 4 的进度事件、上下文预算或并行工具调用。

## 观测字段

`Observation` 除既有的 `summary`、`error` 和 `data` 外，还会由 runtime 写入：

| 字段 | 含义 |
| --- | --- |
| `check_status` | 检查动作的执行结果：`completed`、`failed`、`cancelled` 或 `timed_out`。 |
| `target_health` | 本次检查范围内的目标状态：`healthy`、`unhealthy`、`degraded`、`unknown` 或 `not_applicable`。 |
| `target` | 已脱敏的目标身份，例如 HTTP 端点、网络地址、日志文件或容器。 |
| `observed_at` | 工具调用开始时的 UTC 时间。 |
| `facts` | 从脱敏后的结构化工具数据提取出的原子事实，并携带目标和时间。 |

`check_status` 与 `target_health` 不能互相替代。例如，HTTP 500 是
`check_status=completed`、`target_health=unhealthy`；连接被拒绝的工具错误是
`check_status=failed`、`target_health=unknown`。后者不足以单独证明依赖已宕机。

## 最终结论格式

`final.evidence` 仍然是报告引用的完整证据目录。新字段使证据角色明确：

- `supporting_evidence`：支持当前结论的 trace 引用，必须也是 `evidence` 的子集。
- `counter_evidence`：削弱或反驳当前结论的 trace 引用，必须也是 `evidence` 的子集，且不能与支持证据重复。
- `pending_verifications`：会改变结论强度、但尚未完成的验证问题；可指定 `target` 和 `suggested_tool`。

可直接作为真实模型输出格式参考的片段：

```json
{
  "type": "final",
  "final": {
    "summary": "登录接口返回 500，错误日志可能指向认证分支，但尚未完成请求关联。",
    "root_cause": {
      "status": "suspected",
      "statement": "认证分支异常仍是待验证假设。"
    },
    "evidence": [
      {"step": 1, "tool": "http_check", "summary": "HTTP 500"},
      {"step": 2, "tool": "log_read", "summary": "认证错误日志"}
    ],
    "supporting_evidence": [
      {"step": 1, "tool": "http_check", "summary": "HTTP 500"}
    ],
    "counter_evidence": [
      {"step": 2, "tool": "log_read", "summary": "日志未能关联请求"}
    ],
    "pending_verifications": [
      {
        "question": "同一 request_id 是否命中认证错误分支？",
        "reason": "该关联结果会确认或排除当前假设。",
        "suggested_tool": "log_read"
      }
    ]
  }
}
```

## 结论强度

| 状态 | runtime 强制条件 |
| --- | --- |
| `identified` | 必须有 `fault_type`、根因证据、非空 `supporting_evidence`；根因证据必须是支持证据；不得包含 `counter_evidence`；还必须满足该故障类型的最低证据规则。 |
| `suspected` | 必须有结论文本、非空 `supporting_evidence` 和至少一项 `pending_verifications`。 |
| `undetermined` | 用于当前证据不能区分原因的情况；可以保留支持、反证和后续验证，但不把它们升级成已识别根因。 |

支持的 `identified` 故障类型及最低必要证据：

| `fault_type` | 最低证据 |
| --- | --- |
| `application_error_log` | 已完成且 `unhealthy` 的 `http_check`，以及已完成的 `log_read`。 |
| `dependency_unavailable` | 已完成且 `unhealthy` 的直接依赖检查（PostgreSQL、Redis 或 Kafka）。 |
| `redis_instance_mismatch` | 已完成的 `redis_check`，并有 `instance_match=false` 事实。 |
| `websocket_handshake_rejected` | 已完成且 `unhealthy` 的 `websocket_check`。 |
| `kafka_consumer_stall` | 已完成、`degraded` 或 `unhealthy` 的 `kafka_check`，且 `active_lag > 0`。 |

这些规则是必要条件，不会从自由文本、HTTP 状态码或连接错误自动推导额外事实。
规则不满足时，必须降级为 `suspected` 或 `undetermined`。

## 报告和兼容性

Markdown 报告会展示支持证据、反证、待验证事项和被 `final.evidence` 引用的
结构化事实。报告内容仍会回查 trace，而不是采信模型提供的 `summary`。

读取阶段 3 之前的运行记录时，缺失的 `check_status` 继续按旧规则兼容：trace 有
`error` 视为失败，否则视为已完成。旧 trace 不会被迁移或改写；由新 runtime 执行的
工具调用才会写入新字段。旧的 `identified` 最终报告可以继续读取和生成，但新的模型
输出需要遵守本页的更严格约束。

## 已知限制

- 常见故障规则只验证结构化的最低证据，不进行自然语言语义匹配，也不替代当前运行的证据审查。
- 目标身份优先使用安全、可重建的地址或名称；无法安全解析时会退回工具级身份，绝不持久化完整凭据或 DSN。
- `redis_instance_mismatch` 需要上游检查产生 `instance_match=false` 事实；阶段 3 不擅自引入环境期望值或实例比对配置。
