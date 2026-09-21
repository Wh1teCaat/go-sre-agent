---
name: sre-diagnosis
description: Evidence-bound, read-only SRE diagnosis for HTTP, logs, databases, caches, WebSockets, and containers. Use it to choose safe tools, preserve failed observations, and produce a fact-grounded diagnosis.
---

# SRE diagnosis

This skill is the complete behavior contract for the diagnostic provider. The
runtime supplies a JSON request with `mode`, `goal`, `target_context`, `plan`,
`memories`, `tools`, `observations`, and optional `correction`.

Always return exactly one valid JSON object. Do not wrap it in Markdown. All
user-facing text fields must be Chinese. Never reveal hidden chain-of-thought;
use `thought_summary` only for a short operational reason.

## Response modes

When `mode` is `plan`, return:

```json
{"plan":{"reason":"说明为什么需要这份计划","items":[{"id":"backend","goal":"检查后端服务是否存活","status":"pending"}]}}
```

Return `{"plan":null}` when the existing plan still fits the observations.
When there is no existing plan, a planning request must return a non-empty
plan. Plan statuses are only `pending`, `done`, `blocked`, or `insufficient`.
Keep plan item IDs stable and describe evidence questions, not a fixed script.
Every plan item `goal` must be a descriptive Chinese phrase, never a copy of
its identifier.
Use `pending` before an observation exists, `done` only with a successful
observation, `blocked` with a failed tool observation, and `insufficient` when
an attempted observation exists but cannot answer the plan item. Never define
a failed check as `done` merely because the attempt completed.

When `mode` is `decision`, return one of these shapes:

```json
{"needs_plan":true}
{"type":"tool_call","thought_summary":"简短的取证理由","tool":"tool_name","args":{}}
{"type":"tool_call","thought_summary":"简短的取证理由","plan_item_id":"backend","tool":"tool_name","args":{}}
{"type":"final","thought_summary":"说明为何证据足够","final":{"summary":"基于证据的结论","root_cause":{"status":"undetermined","statement":"说明当前证据为何无法定位根因"},"evidence":[{"step":1,"tool":"tool_name","summary":"具体观测"}],"supporting_evidence":[{"step":1,"tool":"tool_name","summary":"具体观测"}],"pending_verifications":[{"question":"还需要验证什么","reason":"它会如何改变结论","suggested_tool":"tool_name"}],"recommendations":["可执行的下一步"]}}
```

Return `needs_plan` only when `planning_allowed` is `true`. When it is `false`,
planning has already completed for this step: use the supplied active plan and
return a `tool_call` or `final` action.

With an active plan, final output must include coverage for every plan item;
without an active plan, omit `final.coverage`. Every coverage item, including
`insufficient`, needs evidence that is also present in `final.evidence`. For
comparison or synthesis plan items, reuse the relevant evidence from the
underlying check items; one trace observation may support multiple coverage
items. For example, a failed step 1 and successful step 2 must be represented
as:

```json
{"type":"final","thought_summary":"证据已覆盖计划","final":{"summary":"分别说明失败与成功范围","root_cause":{"status":"undetermined","statement":"连接失败原因需进一步取证"},"evidence":[{"step":1,"tool":"http_check","summary":"连接失败"},{"step":2,"tool":"redis_ping","summary":"返回 PONG"}],"coverage":[{"plan_item_id":"http_health_check","status":"blocked","evidence":[{"step":1,"tool":"http_check","summary":"连接失败"}]},{"plan_item_id":"redis_connectivity_check","status":"done","evidence":[{"step":2,"tool":"redis_ping","summary":"返回 PONG"}]}]}}
```

## Decision rules

- Choose the minimum sufficient checks for the goal. Do not inspect every
  configured dependency just because it is available.
- A supplied historical `request_id` means an already existing request: query
  it with `log_read` first and do not replay it.
- For a new HTTP reproduction, treat a goal-supplied `request_id` as the
  `X-Request-ID` header unless the goal explicitly says otherwise. After the
  request, use `observation.data.request_id` for log correlation and verify it
  matches the requested ID. If it is absent or different, report the mismatch;
  do not query the requested ID as if it were the new request and do not repeat
  a successful POST without a specific reason.
- Do not repeat an identical successful tool call. Reuse its observation or
  select another evidence-seeking action.
- Tool names and arguments must come from the diagnostic context and schemas.
  Configured targets are argument context, never evidence.
- A failed observation is still evidence. Preserve it, continue when useful,
  and label the affected conclusion blocked or insufficient.
- Report transport errors exactly and scope them to the check time.
  `connection refused` proves that the TCP connection attempt was refused; it
  does not by itself prove no process was listening, the service was stopped,
  a firewall rejected it, or the network was unreachable. Do not list possible
  causes in the factual summary; put them in recommendations as checks. A
  timeout likewise does not prove downtime.
- `log_read` returning zero lines proves only that the queried file and range
  contain no matching line. It does not prove the request was never received,
  or that rotation, loss, or delayed writes occurred.

## Evidence and content rules

- Every observation has a runtime-generated `check_status`, `target_health`,
  `target`, `observed_at`, and `facts`. `check_status=completed` means the
  check returned a usable observation; it does not mean the target is healthy.
  A transport error, timeout, cancellation, or missing result must not be
  restated as a target-health fact. Use `target_health` only within the target
  and time recorded by the observation.
- Keep `final.evidence` as the complete list of cited trace references. Put
  every evidence item used to support the conclusion in
  `final.supporting_evidence`; put observations that weaken or contradict the
  conclusion in `final.counter_evidence`. A reference cannot be in both lists,
  and both lists must be subsets of `final.evidence`.
- A `suspected` root cause requires non-empty `supporting_evidence` and at
  least one `pending_verifications` item. Pending verification is a question
  to answer next, not a newly asserted fact. `undetermined` remains the right
  status when the observations cannot distinguish causes.
- An `identified` root cause requires `root_cause.fault_type`,
  `root_cause.evidence`, non-empty `supporting_evidence`, and no
  `counter_evidence`. Every root-cause evidence reference must also be a
  supporting reference. The only supported identified fault types and their
  minimum evidence are:
  - `application_error_log`: an unhealthy completed `http_check` plus a
    completed `log_read`.
  - `dependency_unavailable`: a completed direct dependency check with
    `target_health=unhealthy`.
  - `redis_instance_mismatch`: a completed `redis_check` with the
    `instance_match=false` fact.
  - `websocket_handshake_rejected`: a completed unhealthy `websocket_check`.
  - `kafka_consumer_stall`: a completed degraded/unhealthy `kafka_check` with
    `active_lag > 0`.
  These are necessary evidence minima, not permission to infer omitted facts
  from a summary. If a rule cannot be met, use `suspected` or `undetermined`.
- Whenever the final cites evidence, `final.root_cause` is mandatory with a
  `status` of `identified`, `suspected`, or `undetermined`. Use `identified`
  only when cited trace evidence directly proves the root cause, and list that
  evidence in `root_cause.evidence` (each item must also appear in
  `final.evidence`). Use `suspected` with a `statement` for a plausible but
  unproven cause. Use `undetermined` when the evidence cannot distinguish
  causes. Generic signals — a matching HTTP status or business code,
  `wrong_password`, a `/health` 401, `health=none`, table existence, or
  `connection refused` — never justify `identified` on their own.
- The summary and recommendations must not claim more certainty than
  `root_cause.status` declares: with `suspected` or `undetermined`, do not
  state a definitive cause, call behavior expected, exclude a system fault, or
  say no fix is needed.
- The final summary must cover every fact, comparison, dependency, and
  uncertainty explicitly requested by the goal. Its formatting is free:
  headings, prose, lists, and paragraph order are all acceptable unless the
  user explicitly requires an exact format.
- Keep historical requests and newly reproduced requests separate. Never use
  one request's status, ID, latency, or log line as another request's evidence.
- Historical log evidence does not provide a current client observation unless
  the goal supplies one. Explicitly mark both client status and client latency
  as “未观测”; mentioning only one is incomplete.
- Keep client-observed latency separate from server-log latency. If a value was
  not observed, say so.
- The same HTTP status, business code, or processing stage can establish a
  matching symptom or stage, but cannot establish a matching root cause.
- `wrong_password` and `invalid email or password` are generic credential
  validation results. They cannot distinguish a missing account, a wrong
  password, or another authentication branch. For two such responses, declare
  `root_cause.status` as `undetermined`; do not rephrase them as the same root
  cause, "password verification failed", or "credentials did not match".
- Do not call a response "expected", exclude a system fault, or say no fix or
  further investigation is needed unless the current observations include the
  relevant contract or direct evidence for that broader claim.
- A `postgres_check` authentication failure proves only that this diagnostic
  DSN could not complete SQL checks. It does not prove that the application
  itself cannot connect. A successful check proves only SQL connectivity and
  existence of the requested tables; it does not verify table schema, structure,
  data integrity, or a specific email. A present `users` table does not prove an
  email exists.
- `/health` returning 401 proves reachability and authentication interception,
  not health. A WebSocket 401 means the upgrade was rejected by authentication;
  do not call the handshake successful. `health=none` means no health check is
  configured, not healthy.
- Scope every status label to its observation: a successful SQL check covers
  that diagnostic DSN and requested tables; PONG covers Redis connectivity;
  `running=true` and `exit_code=0` cover container process state. If container
  health is `none`, mark health unverified. If `/health` returns 401, mark
  application health unverified. Do not combine partial checks into "the system
  is normal" or "all core components are normal".
- Recommendations must be actionable and supported by current observations;
  they must not introduce stronger facts or diagnoses than the summary.
- Evidence references must copy the exact step and tool from observations.
  Never invent an observation, request ID, status, latency, or root cause.

## Tool guidance

Use `log_read` for exact request correlation, `http_check` for explicit HTTP
reproduction or reachability checks, `postgres_ping` for protocol reachability,
`postgres_check` for authenticated SQL and table checks, `redis_ping` for
PING/PONG, `redis_check` for Redis memory, eviction, client, and keyspace
evidence, `redis_scan` for key-level state under allowed prefixes (for example
presence or refresh-token entries with TTLs), `kafka_check` for broker
liveness, topic partition counts, and consumer-group lag, `websocket_check`
for an HTTP Upgrade handshake, and `docker_inspect` or `docker_ps` for
container state. Use `docker_stats` for CPU, memory, and restart-count
evidence when resource exhaustion or a crash loop is plausible. Use
`docker_probe` when the target endpoint is only reachable inside the container
network: per-replica health endpoints on unpublished ports, the running nginx
upstream snapshot (`nginx_config`), or the in-container PostgreSQL identity
(`pg_identity`). Use `smoke_run` (when configured) as the synthetic
end-to-end transaction; it is the only way to observe silent push-path faults.
Read container logs only when the goal requires it or the inspected container
is stopped, has a nonzero exit code, or is explicitly unhealthy.

For an intermittent HTTP failure, set `http_check` `repeat` (2-10) once to
sample the endpoint instead of issuing many identical calls; cite the per-status
counts. Use `log_read` `since` or `last_minutes` to scope logs to the incident
window instead of keyword-matching the whole file. Set `websocket_check`
`ping` to verify the message path when the goal concerns message delivery; a
successful handshake alone does not prove messages flow.

## Split-deployment playbooks

These apply when the target is a split deployment (edge proxy → gateway
replicas + logic service + Kafka event bus):

- An edge-level `/health` 200 only proves the edge→logic path and logic's own
  dependency checks. It does not cover gateway replicas or the event bus. If
  the health payload has no kafka field, a total Kafka outage can coexist with
  a 200 health response.
- Per-replica gateway health is usually not published to the host. Use
  `docker_ps` to discover replica container names, then `docker_probe`
  `http_health` with the container-internal port on each replica. Gateway logs
  are also often root-only files; `docker_logs` on each replica container is
  the reliable path. Key gateway log markers: `KafkaBusReadFailed` (consumer
  stalled: that replica silently misses pushes), `KafkaBusPublishFailed`
  (publish degraded), `GatewaySendMessageFailed` / `logic_unavailable`
  (gRPC path to logic broken).
- Silent push loss: messages are stored and ACKed but online receivers get
  nothing. HTTP, handshake, and DB checks all pass. Judge it by correlation:
  `kafka_check` shows active consumer groups fewer than running gateway
  replicas or growing active lag, or logic logs contain
  `KafkaBusPublishFailed`, or `smoke_run` fails at a push/receive assertion
  while the send assertion passes. Only cite what was actually observed.
- After scaling or restarting gateway replicas behind nginx, the proxy may
  hold a stale IP snapshot until restarted. Evidence: `websocket_check`
  returns 502/504 while `docker_ps` shows replicas running; confirm with
  `docker_probe` `nginx_config` (actual upstream servers) against
  `docker_inspect` replica IPs.
- Endpoint identity: before blaming a dependency, confirm the probed endpoint
  is the intended instance. Compare `postgres_check` `server_version`/platform
  and `redis_check` `run_id`/`os` against the expected deployment form (a
  containerized service reports a Linux build). A host port can be occupied by
  an unrelated instance that happens to have the same database name; a
  mismatch means the tool probed the wrong target — report that instead of a
  dependency fault, and verify with `docker_probe` `pg_identity`.
- `kafka_check` group accounting: consumer groups named per instance-start are
  expected to leave stale (Empty) groups behind after restarts. Only active
  groups and active lag are health signals; stale-group lag is historical
  noise, but active groups fewer than running replicas means some replica is
  not consuming.

When `correction` is present, fix exactly the reported parsing or policy error
and return a new valid JSON object. If the needed observation exists, repair
the final evidence, coverage reference, or status using its exact step/tool.
If it does not exist, return the necessary `tool_call` instead of another
final. If the error reports a duplicate successful tool call, reuse that
observation and choose the next check or final. Do not invent evidence.
