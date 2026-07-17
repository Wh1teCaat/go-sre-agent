package logread

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogReadReturnsLatestLinesFromAllowedPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "line 1\nline 2\nline 3\nline 4\n")

	tool := New([]string{dir}, 1000)
	observation, err := tool.Run(context.Background(), mustLogArgs(t, Args{
		Path:  path,
		Lines: 2,
	}))
	if err != nil {
		t.Fatalf("run log read: %v", err)
	}

	lines := observation.Data["lines"].([]string)
	if len(lines) != 2 {
		t.Fatalf("len(lines) = %d, want 2", len(lines))
	}
	if lines[0] != "line 3" || lines[1] != "line 4" {
		t.Fatalf("lines = %#v, want latest two lines", lines)
	}
}

func TestLogReadCapsResultAtConfiguredMaxLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "line 1\nline 2\nline 3\nline 4\n")

	observation, err := New([]string{dir}, 2).Run(context.Background(), mustLogArgs(t, Args{
		Path:  path,
		Lines: 100,
	}))
	if err != nil {
		t.Fatalf("run log read: %v", err)
	}
	lines := observation.Data["lines"].([]string)
	if len(lines) != 2 || lines[0] != "line 3" || lines[1] != "line 4" {
		t.Fatalf("lines = %#v, want configured latest two", lines)
	}
}

func TestLogReadFiltersKeywordBeforeTakingLatestMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "INFO start\nERROR db down\nINFO retry\nERROR redis down\n")

	tool := New([]string{dir}, 1000)
	observation, err := tool.Run(context.Background(), mustLogArgs(t, Args{
		Path:    path,
		Lines:   1,
		Keyword: "ERROR",
	}))
	if err != nil {
		t.Fatalf("run log read: %v", err)
	}

	lines := observation.Data["lines"].([]string)
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	if lines[0] != "ERROR redis down" {
		t.Fatalf("lines = %#v, want latest matching error", lines)
	}
}

func TestLogReadFiltersMultipleKeywordsInOneRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "INFO start\nERROR db down\npanic: crashed\nINFO login ok\n")

	observation, err := New([]string{dir}, 1000).Run(context.Background(), mustLogArgs(t, Args{
		Path:     path,
		Lines:    10,
		Keywords: []string{"ERROR", "panic"},
	}))
	if err != nil {
		t.Fatalf("run log read: %v", err)
	}

	lines := observation.Data["lines"].([]string)
	if len(lines) != 2 || lines[0] != "ERROR db down" || lines[1] != "panic: crashed" {
		t.Fatalf("lines = %#v, want ERROR and panic lines", lines)
	}
}

func TestLogReadFiltersExactRequestID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "2026-07-14T13:04:47Z\twarn\tHTTPBusinessError\t{\"request_id\":\"req-123\",\"error\":\"missing token\"}\n"+
		"2026-07-14T13:04:47Z\tinfo\tHTTPRequest\t{\"request_id\":\"req-123\",\"status\":401}\n"+
		"2026-07-14T13:04:48Z\tinfo\tHTTPRequest\t{\"request_id\":\"req-1234\",\"status\":200}\n")

	observation, err := New([]string{dir}, 1000).Run(context.Background(), mustLogArgs(t, Args{
		Path:      path,
		Lines:     10,
		RequestID: "req-123",
	}))
	if err != nil {
		t.Fatalf("run log read: %v", err)
	}

	lines := observation.Data["lines"].([]string)
	if len(lines) != 2 || !strings.Contains(lines[0], "HTTPBusinessError") || !strings.Contains(lines[1], "HTTPRequest") {
		t.Fatalf("lines = %#v, want complete req-123 flow", lines)
	}
}

func TestLogReadRedactsSensitiveLogLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeLog(t, path, "ERROR password=super-secret api_key=abc123 token: xyz789\n")

	tool := New([]string{dir}, 1000)
	observation, err := tool.Run(context.Background(), mustLogArgs(t, Args{
		Path:  path,
		Lines: 1,
	}))
	if err != nil {
		t.Fatalf("run log read: %v", err)
	}

	lines := observation.Data["lines"].([]string)
	if len(lines) != 1 {
		t.Fatalf("len(lines) = %d, want 1", len(lines))
	}
	for _, leaked := range []string{"super-secret", "abc123", "xyz789"} {
		if strings.Contains(lines[0], leaked) {
			t.Fatalf("log line leaked %q: %s", leaked, lines[0])
		}
	}
	if count := strings.Count(lines[0], "[REDACTED]"); count != 3 {
		t.Fatalf("redaction count = %d, want 3: %s", count, lines[0])
	}
}

func TestLogReadRejectsPathOutsideAllowedDirs(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "app.log")
	writeLog(t, path, "ERROR outside\n")

	tool := New([]string{allowed}, 1000)
	_, err := tool.Run(context.Background(), mustLogArgs(t, Args{
		Path:  path,
		Lines: 10,
	}))
	if err == nil {
		t.Fatal("expected path outside allowed dirs to fail")
	}
}

func writeLog(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

func mustLogArgs(t *testing.T, args Args) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}
