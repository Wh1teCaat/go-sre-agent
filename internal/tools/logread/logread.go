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
	Path    string `json:"path"`
	Lines   int    `json:"lines,omitempty"`
	Keyword string `json:"keyword,omitempty"`
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

func (t *Tool) Name() string {
	return Name
}

func (t *Tool) Description() string {
	return Spec().Description
}

func (t *Tool) Schema() tools.ToolSchema {
	return Spec().Schema
}

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

	lines, err := readLines(path)
	if err != nil {
		return schema.Observation{}, err
	}
	if args.Keyword != "" {
		// 先按关键字过滤，再取尾部 N 行，语义是“最近 N 条匹配日志”。
		lines = filterLines(lines, args.Keyword)
	}
	lines = tail(lines, limit)
	// 日志内容会写入 observation，后续可能进入 prompt 和报告，因此必须在工具层脱敏。
	lines = tools.RedactSensitiveLines(lines)

	summary := fmt.Sprintf("read %d log lines from %s", len(lines), path)
	if args.Keyword != "" {
		summary = fmt.Sprintf("read %d log lines matching %q from %s", len(lines), args.Keyword, path)
	}

	return schema.Observation{
		Tool:    Name,
		Summary: summary,
		Data: map[string]any{
			"path":       path,
			"lines":      lines,
			"lines_read": len(lines),
			"keyword":    args.Keyword,
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

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan log file: %w", err)
	}
	return lines, nil
}

func filterLines(lines []string, keyword string) []string {
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, keyword) {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func tail(lines []string, limit int) []string {
	if limit >= len(lines) {
		return lines
	}
	return lines[len(lines)-limit:]
}

func Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        Name,
		Description: "Read the latest lines from an allowed log file with optional keyword filtering.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"path":    {Type: "string", Required: true, Description: "Log file path under an allowed directory."},
				"lines":   {Type: "number", Description: "Number of latest lines to read."},
				"keyword": {Type: "string", Description: "Optional keyword filter."},
			},
		},
	}
}
