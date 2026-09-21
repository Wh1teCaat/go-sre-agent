package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/y2/go-sre-agent/internal/agent"
)

// cliProgressWriter 将 runtime 结构化事件转为面向人的 stderr 输出。锁保证未来
// runtime 改为异步发布事件时，单条进度行也不会交错。
type cliProgressWriter struct {
	writer io.Writer
	mutex  sync.Mutex
}

// newCLIProgressWriter 创建只写入指定输出流的进度接收器。
func newCLIProgressWriter(writer io.Writer) *cliProgressWriter {
	return &cliProgressWriter{writer: writer}
}

// Report 仅格式化进度，不输出诊断报告或机器可读结果。
func (p *cliProgressWriter) Report(event agent.ProgressEvent) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	switch event.Kind {
	case agent.ProgressCheckStarted:
		message := strings.TrimSpace(event.Message)
		if message == "" {
			message = "检查 " + event.Tool
		}
		fmt.Fprintf(p.writer, "[%d/%d] 正在%s…\n", event.Step, event.TotalSteps, message)
	case agent.ProgressCheckCompleted:
		result := strings.TrimSpace(event.Summary)
		if strings.TrimSpace(event.Error) != "" {
			result = event.Error
		}
		if result == "" {
			result = "未返回摘要"
		}
		fmt.Fprintf(p.writer, "[%d/%d] 检查完成：%s，耗时 %s\n", event.Step, event.TotalSteps, result, displayDuration(event.Duration))
	case agent.ProgressFinalizing:
		fmt.Fprintln(p.writer, "正在整理诊断报告…")
	}
}

// displayDuration 将小于一毫秒的耗时也显示为可读的正值。
func displayDuration(duration time.Duration) time.Duration {
	if duration <= 0 {
		return time.Millisecond
	}
	if duration < time.Millisecond {
		return time.Millisecond
	}
	return duration.Round(time.Millisecond)
}
