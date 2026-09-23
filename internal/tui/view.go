package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

var (
	subtle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bright = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	rule   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (m *Model) resize() {
	w := max(1, m.width)
	inputHeight := 1
	if m.height < 8 {
		inputHeight = 1
	}
	if m.height >= 8 && strings.Contains(m.input.Value(), "\n") {
		inputHeight = min(6, 1+strings.Count(m.input.Value(), "\n"))
	}
	m.input.SetWidth(max(1, w-2))
	m.input.SetHeight(inputHeight)
	candidateHeight := min(5, len(m.candidates))
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
			for _, c := range m.checks {
				mark := "⠋"
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
			if len(m.checks) == 0 {
				body.WriteString(" ⠋ Preparing checks...\n")
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
		footer = fmt.Sprintf("Diagnosing · %s    Ctrl-C Cancel    Ctrl-O Details", time.Since(m.started).Round(time.Second))
	} else if m.commandBusy {
		footer = fmt.Sprintf("Running command · %s    Ctrl-C Cancel", time.Since(m.commandStarted).Round(time.Second))
	}
	if m.cancelling {
		footer = "Cancelling and saving..."
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
