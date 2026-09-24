package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	subtle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bright = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	rule   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// Three lit Braille dots rotate through the corners of a two-by-two square.
var squareSpinnerFrames = [...]string{"⠋", "⠙", "⠚", "⠓"}

func (m Model) squareSpinnerMark() string {
	return squareSpinnerFrames[m.spinnerFrame%len(squareSpinnerFrames)]
}

func (m *Model) resize() {
	w := max(1, m.width)
	previousWidth, previousHeight := m.input.Width(), m.input.Height()
	m.input.SetWidth(max(1, w-2))
	candidateHeight := min(5, len(m.candidates))
	inputHeight := 1
	if m.height >= 8 {
		available := max(1, m.height-6-candidateHeight)
		inputHeight = min(6, available, inputRows(m.input.Value(), m.input.Width()))
	}
	m.input.SetHeight(inputHeight)
	if m.input.Width() != previousWidth || inputHeight != previousHeight {
		m.repositionInput()
	}
	h := m.height - 5 - inputHeight - candidateHeight
	if m.height < 8 {
		h = m.height - 2 - inputHeight
	}
	if h < 1 {
		h = 1
	}
	m.viewport.Width = w
	m.viewport.Height = h
	m.refresh()
}

// textarea keeps its old scroll offset when SetHeight grows the viewport.
// Rebuild it at the new size, then restore the draft cursor before rendering.
func (m *Model) repositionInput() {
	value := m.input.Value()
	line := m.input.Line()
	info := m.input.LineInfo()
	column := info.StartColumn + info.ColumnOffset
	m.input.SetValue(value)
	for steps := 0; m.input.Line() > line && steps <= len([]rune(value)); steps++ {
		m.input.CursorUp()
	}
	m.input.SetCursor(column)
	_ = m.input.View()
	m.input, _ = m.input.Update(tea.KeyMsg{})
}

func inputRows(value string, width int) int {
	if width < 1 {
		return 1
	}
	rows := 0
	for _, line := range strings.Split(value, "\n") {
		rows += wrappedInputRows(line, width)
		if rows >= 6 {
			return 6
		}
	}
	return max(1, rows)
}

// Match textarea's word wrapping, including its extra cursor row at an exact edge.
func wrappedInputRows(line string, width int) int {
	rows, lineWidth := 1, 0
	word := make([]rune, 0, len(line))
	spaces := 0
	for _, r := range line {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}
		if spaces > 0 {
			wordWidth := ansi.StringWidth(string(word))
			if lineWidth+wordWidth+spaces > width {
				rows++
				lineWidth = wordWidth + spaces
			} else {
				lineWidth += wordWidth + spaces
			}
			word = word[:0]
			spaces = 0
		} else if len(word) > 0 && ansi.StringWidth(string(word))+ansi.StringWidth(string(word[len(word)-1])) > width {
			if lineWidth > 0 {
				rows++
			}
			lineWidth = ansi.StringWidth(string(word))
			word = word[:0]
		}
	}
	if lineWidth+ansi.StringWidth(string(word))+spaces >= width {
		rows++
	}
	return rows
}

func (m *Model) refresh() {
	follow := m.viewport.AtBottom() || m.viewport.TotalLineCount() == 0
	var body strings.Builder
	width := max(1, m.viewport.Width-4)
	selectedTop, selectedBottom := -1, -1
	if m.overlay != "" {
		body.WriteString(bright.Render(safe(m.overlayTitle)) + "\n\n" + wrap(safe(m.overlay), width))
		if m.confirm {
			cancel, accept := "[Cancel]", " Confirm "
			if m.confirmYes {
				cancel = " Cancel "
				accept = "[Confirm]"
			}
			body.WriteString("\n\n" + cancel + "   " + accept)
		}
		if len(m.picker) > 0 {
			line := strings.Count(body.String(), "\n") + 2
			body.WriteString("\n\n")
			choices := m.filteredChoices()
			if len(choices) == 0 {
				body.WriteString("No matches\n")
			}
			for i, c := range choices {
				mark := "  "
				if i == m.pickerSelection {
					mark = "› "
				}
				rendered := mark + wrap(safe(c.Label), width-2)
				lines := 1 + strings.Count(rendered, "\n")
				if i == m.pickerSelection {
					selectedTop, selectedBottom = line, line+lines-1
				}
				body.WriteString(rendered + "\n")
				line += lines
			}
		}
	} else {
		for _, msg := range m.messages {
			prefix := "  "
			switch msg.Role {
			case "user":
				prefix = " › "
			case "memory":
				prefix = " ◦ "
			case "assistant":
				prefix = "  "
			}
			body.WriteString(prefix + wrap(safe(msg.Text), width) + "\n\n")
		}
		if m.active {
			body.WriteString(" Check progress\n")
			pendingCheck := false
			for _, c := range m.checks {
				if !c.done {
					pendingCheck = true
				}
				mark := m.squareSpinnerMark()
				if c.done {
					mark = "✓"
				}
				summary := c.summary
				if c.err != "" {
					summary = c.err
				}
				duration := ""
				if c.done {
					duration = "  " + c.duration.Round(time.Millisecond).String()
				}
				body.WriteString(wrap(fmt.Sprintf(" %s %s  %s%s", mark, safe(c.tool), safe(summary), duration), max(1, m.viewport.Width-1)) + "\n")
			}
			if !pendingCheck || m.cancelling {
				body.WriteString(" " + m.activityDisplay() + "\n")
			}
		}
	}
	m.viewport.SetContent(body.String())
	if selectedTop >= 0 {
		if selectedTop < m.viewport.YOffset || selectedBottom-selectedTop+1 >= m.viewport.Height {
			m.viewport.SetYOffset(selectedTop)
		} else if selectedBottom >= m.viewport.YOffset+m.viewport.Height {
			m.viewport.SetYOffset(selectedBottom - m.viewport.Height + 1)
		}
		return
	}
	if follow {
		m.viewport.GotoBottom()
		m.newMessages = false
	}
}
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing terminal..."
	}
	if m.height < 8 {
		header := lipgloss.NewStyle().MaxWidth(max(1, m.width)).Render(" sre · " + safe(m.header.Environment))
		return header + "\n" + m.viewport.View() + "\n" + m.input.View()
	}
	line := rule.Render(strings.Repeat("─", max(1, m.width)))
	header := fmt.Sprintf(" sre  ·  %s  ·  %s  ·  %s", safe(m.header.Environment), safe(m.header.Model), safe(m.header.SessionID))
	header = lipgloss.NewStyle().MaxWidth(m.width).Render(header)
	var b strings.Builder
	b.WriteString(header + "\n" + line + "\n")
	b.WriteString(m.viewport.View() + "\n" + line + "\n")
	if len(m.candidates) > 0 && m.overlay == "" {
		for _, s := range m.candidates[:min(5, len(m.candidates))] {
			b.WriteString(" " + bright.Render(s) + "\n")
		}
	}
	b.WriteString(m.input.View() + "\n" + line + "\n")
	footer := "Ready  ·  Ctrl-D Exit  ·  Ctrl-O Details"
	if m.active {
		footer = fmt.Sprintf("%s · %s    Ctrl-C Cancel    Ctrl-O Details", m.activityDisplay(), time.Since(m.started).Round(time.Second))
	} else if m.commandBusy {
		footer = fmt.Sprintf("Running command · %s    Ctrl-C Cancel", time.Since(m.commandStarted).Round(time.Second))
	}
	if m.overlay != "" {
		footer = "Esc or q Close  ·  PgUp/PgDn Scroll"
		if m.confirm {
			footer = "Esc Cancel  ·  Enter Select"
		} else if len(m.picker) > 0 {
			count := len(m.filteredChoices())
			footer = fmt.Sprintf("%d/%d  ·  Up/Down Select  ·  Enter Open  ·  Esc Cancel", min(m.pickerSelection+1, count), count)
		}
	}
	if m.newMessages {
		footer += "    New messages"
	}
	if m.notice != "" {
		footer += "    " + safe(m.notice)
	}
	b.WriteString(subtle.Render(lipgloss.NewStyle().MaxWidth(m.width).Render(" " + footer)))
	return b.String()
}

// activityDisplay is also used in the fixed footer, so the current phase stays
// visible when the check list is longer than the viewport.
func (m Model) activityDisplay() string {
	label := m.activityLabel()
	if strings.HasPrefix(label, "Thinking.") {
		return label
	}
	return m.squareSpinnerMark() + " " + label
}

func (m Model) activityLabel() string {
	switch {
	case m.cancelling:
		return "Cancelling and saving..."
	case m.finalizing:
		return "Preparing report..."
	}
	var running []string
	for _, c := range m.checks {
		if !c.done {
			running = append(running, safe(c.tool))
		}
	}
	if len(running) > 0 {
		if len(running) > 2 {
			return fmt.Sprintf("Calling %s, %s +%d more...", running[0], running[1], len(running)-2)
		}
		return "Calling " + strings.Join(running, ", ") + "..."
	}
	if m.modelCallID != "" {
		dots := 1 + (m.spinnerFrame-m.modelStartedFrame)/3%3
		return "Thinking" + strings.Repeat(".", dots)
	}
	if len(m.checks) == 0 {
		return "Preparing checks..."
	}
	return "Waiting for next decision..."
}

func wrap(s string, width int) string {
	if width < 1 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			out = append(out, "")
			continue
		}
		var current strings.Builder
		lineWidth := 0
		for _, r := range line {
			charWidth := lipgloss.Width(string(r))
			if lineWidth+charWidth > width && current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
				lineWidth = 0
			}
			current.WriteRune(r)
			lineWidth += charWidth
		}
		out = append(out, current.String())
	}
	return strings.Join(out, "\n")
}

func (m Model) filteredChoices() []Choice {
	query := strings.ToLower(strings.TrimSpace(m.input.Value()))
	if query == "" {
		return m.picker
	}
	out := make([]Choice, 0, len(m.picker))
	for _, c := range m.picker {
		if strings.Contains(strings.ToLower(c.Label), query) {
			out = append(out, c)
		}
	}
	return out
}
