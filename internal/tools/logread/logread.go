package logread

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const Name = "log_read"

type Args struct {
	Path      string   `json:"path"`
	Lines     int      `json:"lines,omitempty"`
	Keyword   string   `json:"keyword,omitempty"`
	Keywords  []string `json:"keywords,omitempty"`
	RequestID string   `json:"request_id,omitempty"`
}

type Tool struct {
	allowedDirs []string
	maxLines    int
}

func New(allowedDirs []string, maxLines int) *Tool {
	if maxLines <= 0 {
		maxLines = 1000
	}

	cleanDirs := make([]string, 0, len(allowedDirs))
	for _, dir := range allowedDirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			// allowlist 目录先解析软链，后续文件路径也解析软链，
			// 避免通过 symlink 绕出允许读取的日志目录。
			abs = resolved
		}
		cleanDirs = append(cleanDirs, filepath.Clean(abs))
	}

	return &Tool{
		allowedDirs: cleanDirs,
		maxLines:    maxLines,
	}
}

func (t *Tool) Spec() tools.ToolSpec { return Spec() }

func (t *Tool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	select {
	case <-ctx.Done():
		return schema.Observation{}, ctx.Err()
	default:
	}

	var args Args
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode log_read args: %w", err)
	}
	path, err := t.allowedPath(args.Path)
	if err != nil {
		return schema.Observation{}, err
	}

	limit := args.Lines
	if limit <= 0 {
		limit = 100
	}
	if limit > t.maxLines {
		limit = t.maxLines
	}

	keywords := normalizedKeywords(args.Keyword, args.Keywords)
	requestID := strings.TrimSpace(args.RequestID)
	lines, err := readLatestLines(ctx, path, limit, keywords, requestID)
	if err != nil {
		return schema.Observation{}, err
	}
	// 日志内容会写入 observation，后续可能进入 prompt 和报告，因此必须在工具层脱敏。
	lines = tools.RedactSensitiveLines(lines)

	summary := fmt.Sprintf("read %d log lines from %s", len(lines), path)
	if len(keywords) == 1 {
		summary = fmt.Sprintf("read %d log lines matching %q from %s", len(lines), keywords[0], path)
	} else if len(keywords) > 1 {
		summary = fmt.Sprintf("read %d log lines matching %q from %s", len(lines), keywords, path)
	}
	if requestID != "" {
		summary = fmt.Sprintf("read %d log lines for request_id %q from %s", len(lines), requestID, path)
	}

	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data: map[string]any{
			"path":       path,
			"lines":      lines,
			"lines_read": len(lines),
			"keywords":   keywords,
			"request_id": requestID,
		},
	}, nil
}

func (t *Tool) allowedPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("log_read requires path")
	}
	if len(t.allowedDirs) == 0 {
		return "", fmt.Errorf("no allowed log directories configured")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve log path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve log path: %w", err)
	}
	resolved = filepath.Clean(resolved)

	for _, allowedDir := range t.allowedDirs {
		// 使用 filepath.Rel 判断包含关系，避免简单字符串前缀造成
		// /var/log/app2 被误认为在 /var/log/app 内。
		if isInsideDir(resolved, allowedDir) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("log path %q is outside allowed directories", path)
}

func isInsideDir(path string, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// readLatestLines 扫描文件时只保留最后 limit 条匹配行，避免大日志占满内存。
func readLatestLines(ctx context.Context, path string, limit int, keywords []string, requestID string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	ring := make([]string, limit)
	matched := 0
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := scanner.Text()
		if !matchesRequestID(line, requestID) || !matchesAnyKeyword(line, keywords) {
			continue
		}
		ring[matched%limit] = line
		matched++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan log file: %w", err)
	}

	size := min(matched, limit)
	lines := make([]string, size)
	start := 0
	if matched > limit {
		start = matched % limit
	}
	for i := range lines {
		lines[i] = ring[(start+i)%limit]
	}
	return lines, nil
}

func matchesRequestID(line string, requestID string) bool {
	if requestID == "" {
		return true
	}
	start := strings.LastIndex(line, "\t{")
	if start >= 0 {
		start++
	} else {
		start = strings.IndexByte(line, '{')
	}
	if start < 0 {
		return false
	}
	var fields struct {
		RequestID string `json:"request_id"`
	}
	return json.Unmarshal([]byte(line[start:]), &fields) == nil && fields.RequestID == requestID
}

func normalizedKeywords(keyword string, keywords []string) []string {
	all := append([]string(nil), keywords...)
	if keyword != "" {
		all = append(all, keyword)
	}
	result := all[:0]
	for _, item := range all {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func matchesAnyKeyword(line string, keywords []string) bool {
	if len(keywords) == 0 {
		return true
	}
	for _, keyword := range keywords {
		if strings.Contains(line, keyword) {
			return true
		}
	}
	return false
}

func Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Read the latest lines from an allowed log file, optionally filtered by exact request_id and keywords.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"path":       {Type: "string", Required: true, Description: "Log file path under an allowed directory."},
				"lines":      {Type: "number", Description: "Number of latest lines to read."},
				"keyword":    {Type: "string", Description: "Optional keyword filter."},
				"keywords":   {Type: "array", Description: "Optional keyword filters; a line matches when it contains any keyword."},
				"request_id": {Type: "string", Description: "Optional exact request_id from structured JSON log fields; combined with keyword filters."},
			},
		},
	}
}
