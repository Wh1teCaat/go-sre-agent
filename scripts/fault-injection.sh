#!/usr/bin/env bash
# 故障注入回归：注入故障 → 运行诊断 → 断言报告证据链与结构化根因 → 恢复服务。
#
# 前置条件：go-chat 已通过 docker compose 启动，configs/config.yaml 指向本机目标。
# 用法：
#   scripts/fault-injection.sh                # 运行全部场景
#   scripts/fault-injection.sh logic-down     # 只运行单个场景
# 环境变量：
#   CHAT_DIR  go-chat 目录，默认 /home/y2/project/go-chat
#   AGENT_DIR agent 目录，默认为脚本所在仓库根目录
#   SRE_BIN   sre 可执行文件，默认 sre；请先按 README 构建或安装
set -euo pipefail

CHAT_DIR="${CHAT_DIR:-/home/y2/project/go-chat}"
AGENT_DIR="${AGENT_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
SRE_BIN="${SRE_BIN:-sre}"
# 报告与运行状态保留在 .runs 下，失败后可直接用 resume 继续未完成的 run。
WORK_DIR="${WORK_DIR:-$AGENT_DIR/.runs/fault-injection}"
LLM_TIMEOUT="${LLM_TIMEOUT:-60s}"
mkdir -p "$WORK_DIR"

run_agent() {
  local goal="$1" report="$2"
  (cd "$AGENT_DIR" && "$SRE_BIN" diagnose \
    --config configs/config.yaml \
    --max-steps 10 \
    --llm-timeout "$LLM_TIMEOUT" \
    --run-dir "$WORK_DIR/runs" \
    --goal "$goal" \
    --out "$report") || true
}

assert_contains() {
  local report="$1" needle="$2" scenario="$3"
  if ! grep -qF "$needle" "$report"; then
    echo "FAIL [$scenario]: report missing evidence: $needle" >&2
    echo "--- report ---" >&2
    cat "$report" >&2 || true
    exit 1
  fi
}

compose() {
  (cd "$CHAT_DIR" && docker compose "$@")
}

# 无论脚本正常结束还是断言失败退出，都要把注入故障的服务恢复回来。
PENDING_RESTORE=""
restore_pending() {
  if [ -n "$PENDING_RESTORE" ]; then
    compose up -d --no-deps "$PENDING_RESTORE" || true
    PENDING_RESTORE=""
  fi
}
trap restore_pending EXIT

scenario_logic_down() {
  local report="$WORK_DIR/logic-down.md"
  echo "== scenario: logic-down =="
  PENDING_RESTORE=logic
  compose stop logic
  run_agent "诊断 go-chat 登录接口为什么不可用。检查 HTTP 状态、相关容器状态、退出码和最近容器日志，并给出基于本次证据的结论。" "$report"
  assert_contains "$report" '## Root Cause' logic-down
  assert_contains "$report" 'http_check' logic-down
  assert_contains "$report" 'docker_' logic-down
  restore_pending
  echo "PASS logic-down"
}

scenario_redis_down() {
  local report="$WORK_DIR/redis-down.md"
  echo "== scenario: redis-down =="
  PENDING_RESTORE=redis
  compose stop redis
  run_agent "诊断 go-chat 的 Redis 依赖是否可用。检查 Redis 连通性、后端健康接口和最近日志中的缓存错误，并给出基于本次证据的结论。" "$report"
  assert_contains "$report" '## Root Cause' redis-down
  assert_contains "$report" 'redis_' redis-down
  restore_pending
  echo "PASS redis-down"
}

main() {
  local scenario="${1:-all}"
  case "$scenario" in
  logic-down) scenario_logic_down ;;
  redis-down) scenario_redis_down ;;
  all)
    scenario_logic_down
    scenario_redis_down
    ;;
  *)
    echo "unknown scenario: $scenario (logic-down | redis-down | all)" >&2
    exit 2
    ;;
  esac
  echo "all scenarios passed"
}

main "$@"
