package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/y2/go-sre-agent/internal/agent"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

// Backend owns all store, model and tool calls. Its methods run in tea.Cmd goroutines.
// Only the Bubble Tea event loop changes Model state.
type Backend interface {
	Header() Header
	History() []Message
	Diagnose(context.Context, string, func(agent.ProgressEvent)) Completion
	Resume(context.Context, string, bool, func(agent.ProgressEvent)) Completion
	Command(context.Context, string) CommandResult
	CancelPending()
}

type Header struct{ Environment, Model, SessionID string }
type Message struct{ Role, Text string }
type Completion struct {
	RunID, SessionID, Status, Summary, Health, HealthDetail, Issue, Cause, ReportPath string
	Evidence                                                                          []schema.Evidence
	Recommendations                                                                   []string
	Collected                                                                         bool
	MemoryLoaded                                                                      bool
	Memories                                                                          []schema.Memory
	Err                                                                               error
}
type Choice struct{ Label, Value string }

type CommandResult struct {
	Text, Title            string
	SessionID              string
	Pending, Confirm, Exit bool
	Choices                []Choice
	ChoiceCommand          string
	ResumeID               string
	ResumeRunning          bool
	DiagnoseGoal           string
	ResetTranscript        bool
	History                []Message
}
type eventMsg struct {
	task  uint64
	event agent.ProgressEvent
}
type doneMsg struct {
	task   uint64
	result Completion
}
type commandMsg struct{ result CommandResult }
type tickMsg time.Time
type terminateMsg struct{}
type memoryStatusMsg string

type check struct {
	key, tool, summary, err string
	duration                time.Duration
	done                    bool
}

type Model struct {
	backend               Backend
	header                Header
	width, height         int
	input                 textarea.Model
	viewport              viewport.Model
	messages              []Message
	checks                []check
	memories              []schema.Memory
	checkIndex            map[string]int
	history               []string
	historyIndex          int
	active                bool
	commandBusy           bool
	exitAfterCancel       bool
	cancelling            bool
	finalizing            bool
	modelCallID           string
	task                  uint64
	memorySeenTask        uint64
	cancel                context.CancelFunc
	events                chan tea.Msg
	done                  chan tea.Msg
	overlay, overlayTitle string
	savedYOffset          int
	candidates            []string
	candidateIndex        int
	picker                []Choice
	pickerAction          string
	pickerSelection       int
	confirm               bool
	confirmYes            bool
	pendingResumeID       string
	pendingResumeRunning  bool
	pending               bool
	newMessages           bool
	spinnerFrame          int
	modelStartedFrame     int
	started               time.Time
	commandStarted        time.Time
	notice                string
}

var commandNames = []string{"/help", "/sessions", "/use", "/runs", "/resume", "/status", "/plan", "/evidence", "/report", "/config", "/memory", "/eval", "/llm", "/new", "/exit"}

func New(backend Backend) Model {
	in := textarea.New()
	in.Placeholder = "Ask about a service or type /command"
	in.Prompt = "> "
	in.ShowLineNumbers = false
	in.FocusedStyle.CursorLine = in.FocusedStyle.Text
	in.SetHeight(1)
	in.Focus()
	v := viewport.New(80, 16)
	history := backend.History()
	if len(history) > 160 {
		history = history[len(history)-160:]
	}
	messages := make([]Message, 0, len(history))
	for _, msg := range history {
		messages = append(messages, Message{Role: msg.Role, Text: clip(safe(msg.Text), 16*1024)})
	}
	m := Model{backend: backend, header: backend.Header(), input: in, viewport: v, messages: messages, checkIndex: make(map[string]int), events: make(chan tea.Msg, 128), done: make(chan tea.Msg, 1), historyIndex: -1}
	m.refresh()
	return m
}

func (m Model) Init() tea.Cmd { return textarea.Blink }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case memoryStatusMsg:
		m.notice = safe(string(x))
		m.refresh()
		return m, nil
	case terminateMsg:
		if m.active || m.commandBusy {
			m.exitAfterCancel = true
			if m.cancel != nil {
				m.cancel()
			}
			m.notice = "Stopping; cancelling and saving..."
			return m, nil
		}
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width, m.height = x.Width, x.Height
		m.resize()
		return m, nil
	case eventMsg:
		if x.task == m.task && m.active {
			if !m.viewport.AtBottom() {
				m.newMessages = true
			}
			m.progress(x.event)
			m.refresh()
		}
		return m, m.listen()
	case doneMsg:
		if x.task == m.task {
			m.finish(x.result)
			if m.exitAfterCancel {
				return m, tea.Quit
			}
		}
		return m, nil
	case commandMsg:
		m.commandBusy = false
		m.cancel = nil
		if m.exitAfterCancel {
			return m, tea.Quit
		}
		m.pending = x.result.Pending
		m.pendingResumeID = x.result.ResumeID
		m.pendingResumeRunning = x.result.ResumeRunning
		m.confirm = x.result.Confirm
		m.confirmYes = false
		m.picker = x.result.Choices
		m.pickerAction = x.result.ChoiceCommand
		m.pickerSelection = 0
		if x.result.SessionID != "" {
			m.header.SessionID = x.result.SessionID
		}
		if x.result.ResetTranscript {
			m.messages = nil
			m.checks = nil
			m.memories = nil
			m.checkIndex = make(map[string]int)
			m.history = nil
			m.historyIndex = -1
			for _, msg := range x.result.History {
				m.messages = append(m.messages, Message{Role: msg.Role, Text: clip(safe(msg.Text), 16*1024)})
			}
			m.viewport.GotoBottom()
			m.refresh()
		}
		if x.result.Exit {
			return m, tea.Quit
		}
		if x.result.DiagnoseGoal != "" {
			return m, m.start(x.result.DiagnoseGoal)
		}
		if x.result.ResumeID != "" && !x.result.Confirm {
			return m, m.startResume(x.result.ResumeID, false)
		}
		if x.result.Confirm {
			m.openOverlay("Confirm action", x.result.Text)
			m.input.Reset()
		} else if len(x.result.Choices) > 0 {
			m.openOverlay(x.result.Title, x.result.Text)
			m.input.Reset()
		} else if x.result.Title != "" {
			m.openOverlay(x.result.Title, x.result.Text)
		} else if x.result.Text != "" {
			m.appendMessage("system", x.result.Text)
		}
		return m, nil
	case tickMsg:
		if m.active || m.commandBusy {
			m.spinnerFrame++
			if m.active {
				m.refresh()
			}
			return m, tick()
		}
		return m, nil
	case tea.KeyMsg:
		return m.key(x)
	}
	if m.overlay == "" {
		before := m.input.Value()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		if m.input.Value() != before {
			// textarea's Ctrl-V clipboard command returns a separate paste message.
			m.trimPastedNewline()
			m.resize()
		}
		return m, cmd
	}
	return m, nil
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func (m *Model) listen() tea.Cmd {
	return func() tea.Msg {
		select {
		case e := <-m.events:
			return e
		case d := <-m.done:
			return d
		}
	}
}
func (m *Model) appendMessage(role, text string) {
	if !m.viewport.AtBottom() {
		m.newMessages = true
	}
	m.messages = append(m.messages, Message{Role: role, Text: clip(safe(text), 16*1024)})
	if len(m.messages) > 160 {
		m.messages = append([]Message(nil), m.messages[len(m.messages)-160:]...)
	}
	m.refresh()
}
func safe(s string) string {
	s = tools.RedactSensitive(ansi.Strip(s))
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 32 || r == 127 || r >= 0x80 && r <= 0x9f {
			return -1
		}
		return r
	}, s)
}
func (m *Model) progress(e agent.ProgressEvent) {
	switch e.Kind {
	case agent.ProgressModelStarted:
		m.modelCallID = e.CallID
		m.modelStartedFrame = m.spinnerFrame
	case agent.ProgressModelCompleted:
		if e.CallID == m.modelCallID {
			m.modelCallID = ""
		}
	case agent.ProgressMemoryLoaded:
		m.memories = append([]schema.Memory(nil), e.Memories...)
		m.memorySeenTask = m.task
		if len(e.Memories) == 0 {
			m.appendMessage("memory", "No matching history; starting checks.")
		} else {
			var b strings.Builder
			fmt.Fprintf(&b, "Loaded %d relevant history records.", len(e.Memories))
			for _, h := range e.Memories {
				fmt.Fprintf(&b, "\n• Source run %s: %s", h.SourceRunID, h.Subject)
			}
			m.appendMessage("memory", b.String())
		}
	case agent.ProgressCheckStarted, agent.ProgressCheckCompleted:
		key := e.CallID
		if key == "" {
			key = fmt.Sprintf("%d:%s:%s", e.Step, e.Tool, e.PlanItemID)
		}
		i, ok := m.checkIndex[key]
		if !ok {
			i = len(m.checks)
			m.checkIndex[key] = i
			m.checks = append(m.checks, check{key: key, tool: e.Tool})
		}
		c := &m.checks[i]
		if e.Tool != "" {
			c.tool = e.Tool
		}
		if e.Kind == agent.ProgressCheckStarted {
			m.modelCallID = ""
			c.summary = "Calling..."
		}
		if e.Kind == agent.ProgressCheckCompleted {
			c.done = true
			c.summary = clip(e.Summary, 2048)
			c.err = clip(e.Error, 2048)
			c.duration = e.Duration
		}
	case agent.ProgressFinalizing:
		m.modelCallID = ""
		m.finalizing = true
	}
}
func (m *Model) finish(r Completion) {
	if r.MemoryLoaded && m.memorySeenTask != m.task {
		m.progress(agent.ProgressEvent{Kind: agent.ProgressMemoryLoaded, Memories: r.Memories})
	}
	m.active = false
	m.cancelling = false
	m.finalizing = false
	m.modelCallID = ""
	m.cancel = nil
	m.notice = ""
	if r.SessionID != "" {
		m.header.SessionID = r.SessionID
	}
	if r.Err != nil {
		m.appendMessage("system", fmt.Sprintf("Run %s (%s): %v", r.RunID, r.Status, r.Err))
		return
	}
	m.appendMessage("assistant", CompletionText(r))
}

// CompletionText is shared by live results and restored run history.
func CompletionText(r Completion) string {
	var b strings.Builder
	if r.Health != "" {
		b.WriteString("Backend health: " + r.Health)
		if r.HealthDetail != "" {
			b.WriteString(" - " + r.HealthDetail)
		}
	}
	if r.Issue != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Observed issue: " + r.Issue)
	}
	if r.Cause != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Cause: " + r.Cause)
	}
	if r.Summary != "" && r.Summary != r.Issue && r.Summary != r.HealthDetail {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Assessment: " + r.Summary)
	}
	if b.Len() == 0 {
		b.WriteString("Diagnosis completed.")
	}
	b.WriteString("\n\n/report Full report    /evidence Check evidence")
	return b.String()
}
func (m *Model) start(goal string) tea.Cmd {
	m.task++
	m.drainOldEvents()
	id := m.task
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.active = true
	m.cancelling = false
	m.finalizing = false
	m.modelCallID = ""
	m.started = time.Now()
	m.spinnerFrame = 0
	m.notice = ""
	m.checks = nil
	m.memories = nil
	m.checkIndex = make(map[string]int)
	m.appendMessage("user", goal)
	m.input.Reset()
	m.candidates = nil
	m.resize()
	m.history = append(m.history, goal)
	m.historyIndex = -1
	return tea.Batch(tick(), m.listen(), func() tea.Msg {
		defer cancel()
		r := m.backend.Diagnose(ctx, goal, func(e agent.ProgressEvent) {
			select {
			case m.events <- eventMsg{task: id, event: e}:
			default:
			}
		})
		m.done <- doneMsg{task: id, result: r}
		return nil
	})
}

func (m *Model) startResume(runID string, resumeRunning bool) tea.Cmd {
	m.task++
	m.drainOldEvents()
	id := m.task
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.active = true
	m.cancelling = false
	m.finalizing = false
	m.modelCallID = ""
	m.started = time.Now()
	m.spinnerFrame = 0
	m.notice = ""
	m.checks = nil
	m.memories = nil
	m.checkIndex = make(map[string]int)
	m.pendingResumeID = ""
	m.pendingResumeRunning = false
	m.input.Reset()
	m.resize()
	m.overlay = ""
	m.overlayTitle = ""
	m.appendMessage("system", "Resuming run "+runID+"…")
	return tea.Batch(tick(), m.listen(), func() tea.Msg {
		defer cancel()
		r := m.backend.Resume(ctx, runID, resumeRunning, func(e agent.ProgressEvent) {
			select {
			case m.events <- eventMsg{task: id, event: e}:
			default:
			}
		})
		m.done <- doneMsg{task: id, result: r}
		return nil
	})
}

func (m *Model) command(line string) tea.Cmd {
	m.input.Reset()
	m.candidates = nil
	m.resize()
	m.commandBusy = true
	m.commandStarted = time.Now()
	m.notice = ""
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	return tea.Batch(tick(), func() tea.Msg { defer cancel(); return commandMsg{result: m.backend.Command(ctx, line)} })
}
func Run(backend Backend, input io.Reader, output io.Writer) error {
	p := tea.NewProgram(New(backend), tea.WithInput(input), tea.WithOutput(output), tea.WithAltScreen(), tea.WithoutSignalHandler())
	terminating := make(chan os.Signal, 1)
	signal.Notify(terminating, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(terminating)
	stopped := make(chan struct{})
	if source, ok := backend.(interface{ MemoryEvents() <-chan string }); ok {
		if events := source.MemoryEvents(); events != nil {
			go func() {
				for {
					select {
					case <-stopped:
						return
					case status, open := <-events:
						if !open {
							return
						}
						p.Send(memoryStatusMsg(status))
					}
				}
			}()
		}
	}
	go func() {
		select {
		case <-terminating:
			p.Send(terminateMsg{})
		case <-stopped:
		}
	}()
	_, err := p.Run()
	close(stopped)
	return err
}

func clip(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	limit := maxBytes - 32
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && limit < len(s) && s[limit]&0xc0 == 0x80 {
		limit--
	}
	return s[:limit] + "\n...Content truncated; see /evidence or /report for full details."
}

func (m *Model) drainOldEvents() {
	for {
		select {
		case <-m.events:
		default:
			return
		}
	}
}

func (m *Model) openOverlay(title, content string) {
	if m.overlay == "" {
		m.savedYOffset = m.viewport.YOffset
	}
	m.overlayTitle = title
	m.overlay = content
	m.refresh()
	if len(m.picker) == 0 {
		m.viewport.GotoTop()
	}
}
func (m *Model) closeOverlay() {
	m.overlay = ""
	m.overlayTitle = ""
	m.picker = nil
	m.refresh()
	m.viewport.SetYOffset(m.savedYOffset)
}
