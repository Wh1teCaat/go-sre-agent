package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// redactObservation 在 observation 进入 trace、调用记录和后续模型上下文前脱敏。
func redactObservation(observation schema.Observation) schema.Observation {
	observation.Summary = tools.RedactSensitive(observation.Summary)
	observation.Error = tools.RedactSensitive(observation.Error)
	observation.Target.Kind = tools.RedactSensitive(observation.Target.Kind)
	observation.Target.ID = tools.RedactSensitive(observation.Target.ID)
	if observation.Data != nil {
		if data, ok := tools.RedactSensitiveValue(observation.Data).(map[string]any); ok {
			observation.Data = data
		}
	}
	for index := range observation.Facts {
		observation.Facts[index].Key = tools.RedactSensitive(observation.Facts[index].Key)
		observation.Facts[index].Target.Kind = tools.RedactSensitive(observation.Facts[index].Target.Kind)
		observation.Facts[index].Target.ID = tools.RedactSensitive(observation.Facts[index].Target.ID)
		observation.Facts[index].Value = tools.RedactSensitiveValue(observation.Facts[index].Value)
	}
	return observation
}

// enrichObservation 补齐 runtime 可确定的事实元数据。工具只需报告自身观测；
// 检查是否完成、观测时间和调用目标由 runtime 统一记录，避免不同工具语义漂移。
func enrichObservation(observation schema.Observation, toolName string, args map[string]any, startedAt time.Time, ctx context.Context, runErr error, invoked bool, hadResult bool) schema.Observation {
	if observation.CheckStatus == "" {
		observation.CheckStatus = checkStatus(ctx, runErr, invoked, hadResult)
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = startedAt.UTC()
	}
	if observation.Target.Kind == "" || observation.Target.ID == "" {
		observation.Target = targetIdentityForAction(toolName, args)
	}
	if observation.TargetHealth == "" {
		observation.TargetHealth = inferTargetHealth(toolName, observation)
	}
	if len(observation.Facts) == 0 {
		observation.Facts = factsFromObservation(observation)
	} else {
		for index := range observation.Facts {
			if observation.Facts[index].ObservedAt.IsZero() {
				observation.Facts[index].ObservedAt = observation.ObservedAt
			}
			if observation.Facts[index].Target.Kind == "" || observation.Facts[index].Target.ID == "" {
				observation.Facts[index].Target = observation.Target
			}
		}
	}
	return observation
}

// hasObservationResult 判断工具是否在返回错误前提供了可保留的诊断结果。
func hasObservationResult(observation schema.Observation) bool {
	return observation.Tool != "" || observation.Summary != "" || len(observation.Data) > 0 || len(observation.Facts) > 0
}

// checkStatus 只描述检查动作是否获得可用观测，绝不根据目标返回 5xx、缺表等
// 业务结果推断为失败。这样“检查完成但目标不健康”可以被精确表达。
func checkStatus(ctx context.Context, runErr error, invoked bool, hadResult bool) string {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(runErr, context.Canceled) {
		return schema.CheckExecutionCancelled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(runErr, context.DeadlineExceeded) {
		return schema.CheckExecutionTimedOut
	}
	if !invoked || runErr != nil || !hadResult {
		return schema.CheckExecutionFailed
	}
	return schema.CheckExecutionCompleted
}

// inferTargetHealth 只使用完成检查返回的明确协议或状态字段。连接错误、超时等
// 不能区分目标故障与诊断路径故障，因此保守地标为 unknown。
func inferTargetHealth(toolName string, observation schema.Observation) string {
	if observation.CheckStatus != schema.CheckExecutionCompleted {
		return schema.TargetHealthUnknown
	}
	data := observation.Data
	switch toolName {
	case "http_check":
		if status, ok := integerDataValue(data, "status"); ok {
			switch {
			case status >= 500:
				return schema.TargetHealthUnhealthy
			case status >= 400:
				// 401/403 等状态证明可达或被鉴权拦截，但不能证明应用健康。
				return schema.TargetHealthUnknown
			default:
				return schema.TargetHealthHealthy
			}
		}
	case "websocket_check":
		if handshake, ok := boolDataValue(data, "handshake_success"); ok {
			if !handshake {
				return schema.TargetHealthUnhealthy
			}
			if pingOK, exists := boolDataValue(data, "ping_pong_ok"); exists && !pingOK {
				return schema.TargetHealthDegraded
			}
			return schema.TargetHealthHealthy
		}
	case "postgres_ping", "postgres_check", "redis_ping", "redis_check":
		return schema.TargetHealthHealthy
	case "kafka_check":
		if exists, ok := boolDataValue(data, "topic_exists"); ok && !exists {
			return schema.TargetHealthUnhealthy
		}
		if lag, ok := integerDataValue(data, "active_lag"); ok && lag > 0 {
			return schema.TargetHealthDegraded
		}
		return schema.TargetHealthHealthy
	case "log_read", "docker_logs", "docker_stats":
		return schema.TargetHealthNotApplicable
	case "docker_inspect":
		if running, ok := boolDataValue(data, "running"); ok && !running {
			return schema.TargetHealthUnhealthy
		}
	}
	return schema.TargetHealthUnknown
}

// boolDataValue 从工具数据中读取布尔型事实。
func boolDataValue(data map[string]any, key string) (bool, bool) {
	if data == nil {
		return false, false
	}
	value, ok := data[key]
	if !ok {
		return false, false
	}
	result, ok := value.(bool)
	return result, ok
}

// integerDataValue 从工具数据中读取可表示为整数的状态值。
func integerDataValue(data map[string]any, key string) (int, bool) {
	if data == nil {
		return 0, false
	}
	value, ok := data[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		result, err := typed.Int64()
		return int(result), err == nil
	case string:
		result, err := strconv.Atoi(typed)
		return result, err == nil
	default:
		return 0, false
	}
}

// targetIdentityForAction 从工具参数提取不会泄露凭据的目标身份。不能安全解析时，
// 使用工具级标识而不是把原始参数写入 trace。
func targetIdentityForAction(toolName string, args map[string]any) schema.TargetIdentity {
	if rawURL, ok := args["url"].(string); ok && strings.TrimSpace(rawURL) != "" {
		return schema.TargetIdentity{Kind: "endpoint", ID: safeURLIdentity(rawURL)}
	}
	if toolName == "postgres_ping" || toolName == "postgres_check" {
		if dsn, ok := args["dsn"].(string); ok {
			return schema.TargetIdentity{Kind: "postgres", ID: safePostgresIdentity(dsn)}
		}
	}
	if addr, ok := args["addr"].(string); ok && strings.TrimSpace(addr) != "" {
		identity := strings.TrimSpace(addr)
		if topic, ok := args["topic"].(string); ok && strings.TrimSpace(topic) != "" {
			identity += "/" + strings.TrimSpace(topic)
		}
		return schema.TargetIdentity{Kind: "network_service", ID: identity}
	}
	if path, ok := args["path"].(string); ok && strings.TrimSpace(path) != "" {
		return schema.TargetIdentity{Kind: "log_file", ID: strings.TrimSpace(path)}
	}
	if container, ok := args["container"].(string); ok && strings.TrimSpace(container) != "" {
		identity := strings.TrimSpace(container)
		if probe, ok := args["probe"].(string); ok && strings.TrimSpace(probe) != "" {
			identity += "/" + strings.TrimSpace(probe)
		}
		return schema.TargetIdentity{Kind: "container", ID: identity}
	}
	return schema.TargetIdentity{Kind: "diagnostic_tool", ID: toolName}
}

// safeURLIdentity 去除 URL 中的用户信息、查询串和片段后再作为目标身份保存。
func safeURLIdentity(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "configured_endpoint"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// safePostgresIdentity 从 PostgreSQL DSN 提取不含凭据的地址和数据库身份。
func safePostgresIdentity(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if parsed, err := url.Parse(dsn); err == nil && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	values := map[string]string{}
	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if key == "host" || key == "port" || key == "dbname" {
			values[key] = strings.Trim(value, "'\"")
		}
	}
	if values["host"] == "" {
		return "configured_postgres"
	}
	port := values["port"]
	if port == "" {
		port = "5432"
	}
	database := values["dbname"]
	if database == "" {
		database = "default"
	}
	return values["host"] + ":" + port + "/" + database
}

// factsFromObservation 从已脱敏的结构化工具数据提取稳定的原子事实。日志正文、
// 响应片段和命令输出保留在 Data 中，不在 Facts 重复展开，以免制造无界上下文。
func factsFromObservation(observation schema.Observation) []schema.Fact {
	facts := []schema.Fact{{
		Key:        "summary",
		Value:      observation.Summary,
		ObservedAt: observation.ObservedAt,
		Target:     observation.Target,
	}}
	keys := make([]string, 0, len(observation.Data))
	for key := range observation.Data {
		if key == "body_snippet" || key == "lines" || key == "output" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := observation.Data[key]
		if !isFactValue(value) {
			continue
		}
		facts = append(facts, schema.Fact{
			Key:        key,
			Value:      value,
			ObservedAt: observation.ObservedAt,
			Target:     observation.Target,
		})
	}
	return facts
}

// isFactValue 限制自动提升为 Facts 的值类型和字符串长度。
func isFactValue(value any) bool {
	switch typed := value.(type) {
	case nil, bool, int, int64, float64, json.Number:
		return true
	case string:
		return len([]rune(typed)) <= 512
	default:
		return false
	}
}

func redactTraceArgs(args map[string]any) map[string]any {
	redacted, ok := tools.RedactSensitiveValue(args).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return redacted
}
