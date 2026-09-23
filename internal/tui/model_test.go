package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"github.com/y2/go-sre-agent/internal/agent"
	"github.com/y2/go-sre-agent/internal/schema"
)

type fakeBackend struct {
	started   chan string
	cancelled chan struct{}
	completed chan struct{}
	calls     int
}

func (f *fakeBackend) Header() Header {
	return Header{Environment: "local", Model: "mock", SessionID: "ses_test"}
}
func (f *fakeBackend) History() []Message { return nil }
func (f *fakeBackend) Diagnose(ctx context.Context, goal string, emit func(agent.ProgressEvent)) Completion {
	f.calls++
	emit(agent.ProgressEvent{Kind: agent.ProgressMemoryLoaded, Memories: []schema.Memory{{SourceRunID: "run_old", Subject: "历史结论"}}})
	emit(agent.ProgressEvent{Kind: agent.ProgressCheckStarted, CallID: "call_1", Tool: "HTTP", Step: 1})
	f.started <- goal
	if f.calls == 1 {
		<-ctx.Done()
		close(f.cancelled)
		return Completion{RunID: "run_first", Status: "cancelled", Err: ctx.Err()}
	}
	emit(agent.ProgressEvent{Kind: agent.ProgressCheckCompleted, CallID: "call_1", Tool: "HTTP", Summary: "返回 500"})
	close(f.completed)
	return Completion{RunID: "run_second", Status: "completed", Summary: "已完成", Evidence: []schema.Evidence{{Tool: "HTTP", Summary: "返回 500"}}}
}
func (f *fakeBackend) Resume(context.Context, string, bool, func(agent.ProgressEvent)) Completion {
	return Completion{Err: errors.New("unexpected resume")}
}
func (f *fakeBackend) Command(_ context.Context, line string) CommandResult {
	switch line {
	case "/report":
		return CommandResult{Title: "/report", Text: "A saved report"}
	case "/evidence":
		return CommandResult{Title: "/evidence", Text: "Select evidence", Choices: []Choice{{Label: "HTTP", Value: "http"}}, ChoiceCommand: "/evidence-detail"}
	default:
		return CommandResult{}
	}
}
func (f *fakeBackend) CancelPending() {}

func TestProgressCorrelatesParallelCallsAndFiltersControlSequences(t *testing.T) {
	f := &fakeBackend{}
	m := New(f)
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckStarted, CallID: "a", Tool: "HTTP", Message: "内部推理不应显示"})
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckStarted, CallID: "b", Tool: "PostgreSQL"})
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckCompleted, CallID: "b", Tool: "PostgreSQL", Summary: "connection refused"})
	if len(m.checks) != 2 || m.checks[0].done || !m.checks[1].done || m.checks[1].summary != "connection refused" {
		t.Fatalf("checks=%+v", m.checks)
	}
	if got := safe("\x1b[31msecret\x1b[0m\x07"); got != "secret" {
		t.Fatalf("unsafe text=%q", got)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 22, Height: 8})
	m = next.(Model)
	if m.viewport.Height < 1 || strings.Contains(m.View(), "\x1b[31m") || strings.Contains(m.View(), "内部推理不应显示") {
		t.Fatal("small layout or control sequence failed")
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 12, Height: 5})
	m = next.(Model)
	if lines := strings.Count(m.View(), "\n") + 1; lines > 5 {
		t.Fatalf("tiny layout has %d lines", lines)
	}
}

func TestPTYCancelThenNextRunAndRestoreTerminal(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 28, Cols: 90}); err != nil {
		t.Fatal(err)
	}
	f := &fakeBackend{started: make(chan string, 2), cancelled: make(chan struct{}), completed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(f, slave, slave) }()
	output := make(chan string, 128)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				output <- string(buf[:n])
			}
			if err != nil {
				close(output)
				return
			}
		}
	}()
	waitText(t, output, "sre", 5*time.Second)
	if _, err := io.WriteString(master, "first\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case goal := <-f.started:
		if goal != "first" {
			t.Fatalf("goal=%q", goal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not start")
	}
	if _, err := master.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not reach backend")
	}
	waitText(t, output, "cancelled", 5*time.Second)
	if _, err := io.WriteString(master, "second\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case goal := <-f.started:
		if goal != "second" {
			t.Fatalf("next goal=%q", goal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("next run did not start")
	}
	select {
	case <-f.completed:
	case <-time.After(5 * time.Second):
		t.Fatal("next run did not complete")
	}
	waitText(t, output, "已完成", 5*time.Second)
	if _, err := io.WriteString(master, "/exit\r"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not exit")
	}
	waitText(t, output, "\x1b[?1049l", 5*time.Second)
}
func waitText(t *testing.T, ch <-chan string, needle string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var seen strings.Builder
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatalf("terminal closed waiting for %q; output=%q", needle, seen.String())
			}
			seen.WriteString(s)
			if strings.Contains(seen.String(), needle) {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %q; output tail=%q", needle, tail(seen.String()))
		}
	}
}
func tail(s string) string {
	if len(s) > 400 {
		return s[len(s)-400:]
	}
	return s
}

func TestMultilinePasteStaysSingleDraftAndScrollDoesNotJump(t *testing.T) {
	f := &fakeBackend{}
	m := New(f)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 35, Height: 9})
	m = next.(Model)
	pasted := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第一行\n第二行"), Paste: true}
	next, _ = m.key(pasted)
	m = next.(Model)
	if m.input.Value() != "第一行\n第二行" || m.active {
		t.Fatalf("paste draft=%q, active=%v", m.input.Value(), m.active)
	}
	next, _ = m.key(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.active || len(m.messages) != 1 || m.messages[0].Text != "第一行\n第二行" {
		t.Fatalf("submission=%+v", m.messages)
	}
	m.active = false
	for i := 0; i < 20; i++ {
		m.appendMessage("system", "一条较长的历史内容，用于使视图滚动。")
	}
	next, _ = m.key(tea.KeyMsg{Type: tea.KeyPgUp})
	m = next.(Model)
	before := m.viewport.YOffset
	if m.newMessages {
		t.Fatal("new-message marker appeared before a new message")
	}
	m.appendMessage("system", "新的进度消息")
	if !m.newMessages || m.viewport.YOffset != before {
		t.Fatalf("scroll jumped: before=%d after=%d marker=%v", before, m.viewport.YOffset, m.newMessages)
	}
}

func TestSessionSwitchRebuildsTranscript(t *testing.T) {
	m := New(&fakeBackend{})
	m.appendMessage("user", "旧会话内容")
	next, _ := m.Update(commandMsg{result: CommandResult{SessionID: "ses_other", ResetTranscript: true, History: []Message{{Role: "assistant", Text: "已保存的诊断"}}}})
	m = next.(Model)
	if m.header.SessionID != "ses_other" || len(m.messages) != 1 || m.messages[0].Text != "已保存的诊断" {
		t.Fatalf("session switch: header=%+v messages=%+v", m.header, m.messages)
	}
}

func TestSingleLineInputAndReadOnlyViewExit(t *testing.T) {
	m := New(&fakeBackend{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 20})
	m = next.(Model)
	if got := m.input.View(); strings.Contains(got, "\n") {
		t.Fatalf("default input has extra rows: %q", got)
	}
	if strings.Contains(m.View(), "输入问题") || !strings.Contains(m.View(), "Ctrl-D Exit") {
		t.Fatalf("TUI chrome is not English: %q", m.View())
	}
	m.openOverlay("/report", "Report body")
	if !strings.Contains(m.View(), "Esc or q Close") {
		t.Fatalf("report view has no exit hint: %q", m.View())
	}
	next, _ = m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	m = next.(Model)
	if m.overlay != "" {
		t.Fatal("q did not close read-only view")
	}
}

func TestCompletionIsConciseAndHidesMemoryCollection(t *testing.T) {
	m := New(&fakeBackend{})
	m.finish(Completion{Status: "completed", Health: "Unhealthy", HealthDetail: "login returned 500", Issue: "PostgreSQL connection check failed", Cause: "Not confirmed", Collected: true, ReportPath: "reports/run.md"})
	got := m.messages[len(m.messages)-1].Text
	for _, want := range []string{"Backend health: Unhealthy", "Observed issue: PostgreSQL", "Cause: Not confirmed", "/report", "/evidence"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	for _, unexpected := range []string{"memory", "Memory", "Evidence", "Report file:", "reports/run.md"} {
		if strings.Contains(got, unexpected) {
			t.Fatalf("unexpected %q in %q", unexpected, got)
		}
	}
}

func TestPTYReportAndEvidenceCanClose(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	if err := pty.Setsize(slave, &pty.Winsize{Rows: 24, Cols: 90}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Run(&fakeBackend{}, slave, slave) }()
	output := make(chan string, 128)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				output <- string(buf[:n])
			}
			if err != nil {
				close(output)
				return
			}
		}
	}()
	waitText(t, output, "sre", 5*time.Second)
	if _, err := io.WriteString(master, "/report\r"); err != nil {
		t.Fatal(err)
	}
	waitText(t, output, "Esc or q Close", 5*time.Second)
	if _, err := io.WriteString(master, "q"); err != nil {
		t.Fatal(err)
	}
	waitText(t, output, "Ready", 5*time.Second)
	if _, err := io.WriteString(master, "/evidence\r"); err != nil {
		t.Fatal(err)
	}
	waitText(t, output, "1/1", 5*time.Second)
	if _, err := master.Write([]byte{27}); err != nil {
		t.Fatal(err)
	}
	waitText(t, output, "Ready", 5*time.Second)
	if _, err := master.Write([]byte{4}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not exit")
	}
	waitText(t, output, "\x1b[?1049l", 5*time.Second)
}

func TestPickerFollowsSelectionThroughLongWrappedList(t *testing.T) {
	m := New(&fakeBackend{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 26, Height: 12})
	m = next.(Model)
	choices := make([]Choice, 15)
	for i := range choices {
		choices[i] = Choice{Label: fmt.Sprintf("choice_%02d with a long description", i), Value: fmt.Sprintf("%d", i)}
	}
	next, _ = m.Update(commandMsg{result: CommandResult{Title: "Commands", Text: "Select a command", Choices: choices, ChoiceCommand: "/help"}})
	m = next.(Model)
	if !strings.Contains(m.viewport.View(), "choice_00") {
		t.Fatalf("first choice is hidden: %q", m.viewport.View())
	}
	for range 14 {
		next, _ = m.key(tea.KeyMsg{Type: tea.KeyDown})
		m = next.(Model)
	}
	if m.pickerSelection != 14 || m.viewport.YOffset == 0 || !strings.Contains(m.viewport.View(), "choice_14") {
		t.Fatalf("last choice did not scroll into view: selection=%d offset=%d view=%q", m.pickerSelection, m.viewport.YOffset, m.viewport.View())
	}
	if !strings.Contains(m.View(), "15/15") {
		t.Fatalf("picker position is missing: %q", m.View())
	}
	next, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: 9})
	m = next.(Model)
	if !strings.Contains(m.viewport.View(), "choice_14") {
		t.Fatalf("selection hidden after resize: %q", m.viewport.View())
	}
	for range 14 {
		next, _ = m.key(tea.KeyMsg{Type: tea.KeyUp})
		m = next.(Model)
	}
	if m.pickerSelection != 0 || !strings.Contains(m.viewport.View(), "choice_00") {
		t.Fatalf("first choice did not scroll back: selection=%d offset=%d view=%q", m.pickerSelection, m.viewport.YOffset, m.viewport.View())
	}
	next, _ = m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("13")})
	m = next.(Model)
	if len(m.filteredChoices()) != 1 || !strings.Contains(m.viewport.View(), "choice_13") {
		t.Fatalf("filtered choice hidden: %q", m.viewport.View())
	}
}

func TestRunningProgressAnimatesAndRespectsScrollPosition(t *testing.T) {
	m := New(&fakeBackend{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 42, Height: 12})
	m = next.(Model)
	m.active = true
	m.started = time.Now()
	m.refresh()
	if !strings.Contains(m.viewport.View(), "⠋ Preparing checks...") {
		t.Fatalf("initial wait marker missing: %q", m.viewport.View())
	}
	for _, want := range []string{"⠙ Preparing checks...", "⠚ Preparing checks...", "⠓ Preparing checks...", "⠋ Preparing checks..."} {
		next, _ = m.Update(tickMsg(time.Now()))
		m = next.(Model)
		if !strings.Contains(m.viewport.View(), want) {
			t.Fatalf("three-dot square spinner did not rotate to %q: %q", want, m.viewport.View())
		}
	}

	for i := 0; i < 12; i++ {
		next, _ = m.Update(eventMsg{task: m.task, event: agent.ProgressEvent{
			Kind: agent.ProgressCheckStarted, CallID: fmt.Sprintf("call_%d", i), Tool: fmt.Sprintf("check_%d", i),
		}})
		m = next.(Model)
	}
	if !m.viewport.AtBottom() || !strings.Contains(m.viewport.View(), "check_11") {
		t.Fatalf("latest check did not scroll into view: offset=%d view=%q", m.viewport.YOffset, m.viewport.View())
	}
	next, _ = m.key(tea.KeyMsg{Type: tea.KeyPgUp})
	m = next.(Model)
	before := m.viewport.YOffset
	if m.viewport.AtBottom() {
		t.Fatal("PgUp did not move away from the bottom")
	}
	next, _ = m.Update(eventMsg{task: m.task, event: agent.ProgressEvent{
		Kind: agent.ProgressCheckStarted, CallID: "call_new", Tool: "check_new",
	}})
	m = next.(Model)
	next, _ = m.Update(tickMsg(time.Now()))
	m = next.(Model)
	if m.viewport.YOffset != before || !m.newMessages {
		t.Fatalf("progress stole scroll position: before=%d after=%d new=%v", before, m.viewport.YOffset, m.newMessages)
	}
	for !m.viewport.AtBottom() {
		next, _ = m.key(tea.KeyMsg{Type: tea.KeyPgDown})
		m = next.(Model)
	}
	if m.newMessages || !strings.Contains(m.viewport.View(), "check_new") {
		t.Fatalf("returning to bottom did not restore follow: new=%v view=%q", m.newMessages, m.viewport.View())
	}
}

func TestModelThinkingAndCurrentToolStayVisibleInFooter(t *testing.T) {
	m := New(&fakeBackend{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 58, Height: 12})
	m = next.(Model)
	m.active = true
	m.started = time.Now()
	for i := 0; i < 12; i++ {
		m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckCompleted, CallID: fmt.Sprintf("old_%d", i), Tool: "http_check", Summary: "complete"})
	}
	m.progress(agent.ProgressEvent{Kind: agent.ProgressModelStarted, CallID: "model_a"})
	m.refresh()
	if !strings.Contains(m.View(), " Thinking. ·") || !strings.Contains(m.View(), "Ctrl-C Cancel") {
		t.Fatalf("model activity missing or incorrectly prefixed: %q", m.View())
	}
	for _, want := range []string{"Thinking..", "Thinking...", "Thinking."} {
		for range 3 {
			next, _ = m.Update(tickMsg(time.Now()))
			m = next.(Model)
		}
		if got := m.activityDisplay(); got != want {
			t.Fatalf("thinking dots = %q, want %q", got, want)
		}
	}
	next, _ = m.key(tea.KeyMsg{Type: tea.KeyPgUp})
	m = next.(Model)
	before := m.viewport.YOffset
	if m.viewport.AtBottom() {
		t.Fatal("PgUp did not move away from the bottom")
	}
	m.progress(agent.ProgressEvent{Kind: agent.ProgressModelStarted, CallID: "model_b"})
	m.progress(agent.ProgressEvent{Kind: agent.ProgressModelCompleted, CallID: "model_a"})
	m.refresh()
	if m.viewport.YOffset != before || m.activityDisplay() != "Thinking." || !strings.Contains(m.View(), " Thinking. ·") {
		t.Fatalf("thinking state or scroll position lost: %q", m.View())
	}
	m.progress(agent.ProgressEvent{Kind: agent.ProgressModelCompleted, CallID: "model_b"})
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckStarted, CallID: "tool_a", Tool: "docker_logs", Message: "private reasoning"})
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckStarted, CallID: "tool_b", Tool: "http_check"})
	m.refresh()
	view := m.View()
	if !strings.Contains(view, "⠙ Calling docker_logs, http_check...") || strings.Contains(view, "private reasoning") || m.viewport.YOffset != before {
		t.Fatalf("tool activity missing or leaked reasoning: %q", view)
	}
	next, _ = m.Update(tickMsg(time.Now()))
	m = next.(Model)
	if !strings.Contains(m.View(), "⠚ Calling docker_logs, http_check...") || m.viewport.YOffset != before {
		t.Fatalf("tool spinner did not rotate while scrolled: %q", m.View())
	}
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckCompleted, CallID: "tool_a", Tool: "docker_logs", Summary: "complete"})
	m.refresh()
	if !strings.Contains(m.View(), "Calling http_check...") {
		t.Fatalf("remaining tool not shown: %q", m.View())
	}
}

func TestProgressShowsActivityBetweenChecksAndDuringFinalizing(t *testing.T) {
	m := New(&fakeBackend{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 50, Height: 14})
	m = next.(Model)
	m.active = true
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckStarted, CallID: "http", Tool: "HTTP"})
	m.progress(agent.ProgressEvent{Kind: agent.ProgressCheckCompleted, CallID: "http", Tool: "HTTP", Summary: "returned 500"})
	m.refresh()
	if !strings.Contains(m.viewport.View(), "Waiting for next decision...") {
		t.Fatalf("no activity between checks: %q", m.viewport.View())
	}
	m.progress(agent.ProgressEvent{Kind: agent.ProgressFinalizing})
	m.refresh()
	if !strings.Contains(m.viewport.View(), "Preparing report...") {
		t.Fatalf("no activity during finalization: %q", m.viewport.View())
	}
	m.cancelling = true
	m.refresh()
	if !strings.Contains(m.viewport.View(), "Cancelling and saving...") {
		t.Fatalf("no activity during cancellation: %q", m.viewport.View())
	}
}
