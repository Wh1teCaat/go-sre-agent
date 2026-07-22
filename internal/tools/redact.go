package tools

import (
	"regexp"
	"strings"
)

var sensitivePatterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	{
		re:          regexp.MustCompile(`(?i)(authorization\s*:\s*bearer\s+)[^\s]+`),
		replacement: `${1}[REDACTED]`,
	},
	{
		re:          regexp.MustCompile(`(?i)((?:password|token|api[_-]?key|secret)"?\s*[:=]\s*"?)([^"\s,}]+)("?)`),
		replacement: `${1}[REDACTED]${3}`,
	},
}

// RedactSensitive 对常见 token、password、secret、API key 做保守脱敏。
// 它用于日志片段和 prompt 上下文，避免报告或模型请求中泄露敏感值。
func RedactSensitive(text string) string {
	redacted := text
	for _, pattern := range sensitivePatterns {
		redacted = pattern.re.ReplaceAllString(redacted, pattern.replacement)
	}
	return redacted
}

// RedactSensitiveValue 递归脱敏 map、slice 和字符串值，供 trace/observation
// 持久化前使用。参数 value 为待处理的 JSON-like 值；返回不修改原值的副本。
func RedactSensitiveValue(value any) any {
	return redactValue("", value)
}

func redactValue(key string, value any) any {
	if sensitiveKey(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			redacted[childKey] = redactValue(childKey, childValue)
		}
		return redacted
	case map[string]string:
		redacted := make(map[string]string, len(typed))
		for childKey, childValue := range typed {
			if sensitiveKey(childKey) {
				redacted[childKey] = "[REDACTED]"
			} else {
				redacted[childKey] = RedactSensitive(childValue)
			}
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for i, childValue := range typed {
			redacted[i] = redactValue("", childValue)
		}
		return redacted
	case []string:
		redacted := make([]string, len(typed))
		for i, childValue := range typed {
			redacted[i] = RedactSensitive(childValue)
		}
		return redacted
	case string:
		return RedactSensitive(typed)
	default:
		return value
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
	return normalized == "authorization" ||
		normalized == "cookie" ||
		normalized == "dsn" ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "api_key") ||
		strings.Contains(normalized, "secret")
}

// RedactSensitiveLines 返回脱敏后的日志行副本，不修改调用方传入的切片内容。
func RedactSensitiveLines(lines []string) []string {
	redacted := make([]string, 0, len(lines))
	for _, line := range lines {
		redacted = append(redacted, RedactSensitive(line))
	}
	return redacted
}
