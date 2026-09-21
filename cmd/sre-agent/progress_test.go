package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
)

func TestCLIProgressWriterFormatsRuntimeEvents(t *testing.T) {
	var output bytes.Buffer
	writer := newCLIProgressWriter(&output)
	writer.Report(agent.ProgressEvent{
		Kind:       agent.ProgressCheckStarted,
		Step:       1,
		TotalSteps: 12,
		Message:    "检查登录接口",
	})
	writer.Report(agent.ProgressEvent{
		Kind:       agent.ProgressCheckCompleted,
		Step:       1,
		TotalSteps: 12,
		Summary:    "HTTP 500",
		Duration:   86 * time.Millisecond,
	})
	writer.Report(agent.ProgressEvent{Kind: agent.ProgressFinalizing})

	got := output.String()
	for _, want := range []string{
		"[1/12] 正在检查登录接口…",
		"[1/12] 检查完成：HTTP 500，耗时 86ms",
		"正在整理诊断报告…",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("progress output = %q, want %q", got, want)
		}
	}
}

func TestDisplayDurationRoundsSubMillisecondValues(t *testing.T) {
	if got := displayDuration(0); got != time.Millisecond {
		t.Fatalf("zero duration = %s, want 1ms", got)
	}
}
