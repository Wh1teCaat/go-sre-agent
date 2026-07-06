package tools

import "regexp"

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

// RedactSensitiveLines 返回脱敏后的日志行副本，不修改调用方传入的切片内容。
func RedactSensitiveLines(lines []string) []string {
	redacted := make([]string, 0, len(lines))
	for _, line := range lines {
		redacted = append(redacted, RedactSensitive(line))
	}
	return redacted
}
