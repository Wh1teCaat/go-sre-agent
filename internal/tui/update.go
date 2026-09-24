package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m Model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	bulkText := k.Type == tea.KeyRunes && (k.Paste || strings.ContainsAny(string(k.Runes), "\r\n"))
	if bulkText {
		// Bulk text may arrive without a paste flag; ignore only its trailing line breaks.
		k.Runes = []rune(strings.TrimRight(string(k.Runes), "\r\n"))
		if len(k.Runes) == 0 {
			return m, nil
		}
	}
	switch k.String() {
	case "esc":
		if m.commandBusy {
			return m, nil
		}
		m.pendingResumeID = ""
		m.pendingResumeRunning = false
		if m.pending {
			m.backend.CancelPending()
			m.pending = false
			m.confirm = false
			m.notice = "Action cancelled"
		}
		if m.overlay != "" {
			m.closeOverlay()
			m.input.Reset()
			m.resize()
			return m, nil
		}
		if len(m.candidates) > 0 {
			m.candidates = nil
			return m, nil
		}
	case "ctrl+o":
		if m.confirm || len(m.picker) > 0 {
			return m, nil
		}
		if m.overlay != "" {
			m.closeOverlay()
		} else {
			m.openOverlay("Tool details", m.checkDetails())
		}
		return m, nil
	case "ctrl+c":
		if m.commandBusy {
			if m.cancel != nil {
				m.cancel()
				m.notice = "Cancelling command..."
			}
			return m, nil
		}
		if m.active {
			if !m.cancelling {
				m.cancelling = true
				m.notice = "Cancelling and saving..."
				m.cancel()
			}
			return m, nil
		}
		m.input.Reset()
		m.candidates = nil
		m.resize()
		return m, nil
	case "ctrl+d":
		if !m.active && !m.commandBusy && strings.TrimSpace(m.input.Value()) == "" {
			return m, tea.Quit
		}
	case "pgup":
		m.viewport.LineUp(max(1, m.viewport.Height/2))
		return m, nil
	case "pgdown":
		m.viewport.LineDown(max(1, m.viewport.Height/2))
		if m.viewport.AtBottom() {
			m.newMessages = false
		}
		return m, nil
	}
	if m.confirm {
		switch k.String() {
		case "left", "up":
			m.confirmYes = false
		case "right", "down", "tab":
			m.confirmYes = true
		case "enter":
			m.confirm = false
			m.closeOverlay()
			if !m.confirmYes {
				m.pendingResumeID = ""
				m.pendingResumeRunning = false
				m.backend.CancelPending()
				m.pending = false
				m.notice = "Action cancelled"
				return m, nil
			}
			if m.pendingResumeID != "" {
				id := m.pendingResumeID
				return m, m.startResume(id, true)
			}
			return m, m.command("yes")
		}
		return m, nil
	}
	if len(m.picker) > 0 {
		switch k.String() {
		case "up":
			if m.pickerSelection > 0 {
				m.pickerSelection--
			}
			m.refresh()
			return m, nil
		case "down":
			if m.pickerSelection < len(m.filteredChoices())-1 {
				m.pickerSelection++
			}
			m.refresh()
			return m, nil
		case "enter":
			choices := m.filteredChoices()
			if len(choices) > 0 {
				value := choices[m.pickerSelection].Value
				action := m.pickerAction
				m.closeOverlay()
				m.input.Reset()
				return m, m.command(action + " " + value)
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		m.pickerSelection = 0
		m.refresh()
		return m, cmd
	}
	if m.overlay != "" {
		switch k.String() {
		case "q":
			m.closeOverlay()
			return m, nil
		case "up":
			m.viewport.LineUp(1)
		case "down":
			m.viewport.LineDown(1)
		}
		return m, nil
	}
	if m.pending && k.String() == "enter" {
		line := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		m.resize()
		if line == "" {
			m.backend.CancelPending()
			m.pending = false
			m.notice = "Action cancelled"
			return m, nil
		}
		return m, m.command(line)
	}
	if k.String() == "ctrl+j" || k.String() == "alt+enter" {
		m.input.InsertRune('\n')
		m.resize()
		return m, nil
	}
	if k.String() == "tab" && len(m.candidates) > 0 {
		m.input.SetValue(m.candidates[m.candidateIndex] + " ")
		m.candidates = nil
		m.resize()
		return m, nil
	}
	if k.String() == "up" && !strings.Contains(m.input.Value(), "\n") && len(m.history) > 0 {
		if m.historyIndex < 0 {
			m.historyIndex = len(m.history) - 1
		} else if m.historyIndex > 0 {
			m.historyIndex--
		}
		m.input.SetValue(m.history[m.historyIndex])
		m.resize()
		return m, nil
	}
	if k.String() == "down" && !strings.Contains(m.input.Value(), "\n") && m.historyIndex >= 0 {
		if m.historyIndex < len(m.history)-1 {
			m.historyIndex++
			m.input.SetValue(m.history[m.historyIndex])
		} else {
			m.historyIndex = -1
			m.input.Reset()
		}
		m.resize()
		return m, nil
	}
	if k.String() == "enter" {
		line := strings.TrimSpace(m.input.Value())
		if line == "" {
			return m, nil
		}
		if m.active {
			if line == "/exit" || line == "/quit" {
				m.exitAfterCancel = true
				if !m.cancelling {
					m.cancelling = true
					m.notice = "Cancelling and saving..."
					m.cancel()
				}
				return m, nil
			}
			m.notice = "A task is still running"
			return m, nil
		}
		if m.commandBusy {
			if line == "/exit" || line == "/quit" {
				m.exitAfterCancel = true
				if m.cancel != nil {
					m.cancel()
				}
				m.notice = "Cancelling command and exiting..."
				return m, nil
			}
			m.notice = "A command is still running"
			return m, nil
		}
		if strings.HasPrefix(line, "/") {
			if line == "/exit" || line == "/quit" {
				return m, tea.Quit
			}
			return m, m.command(line)
		}
		return m, m.start(line)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	if bulkText {
		m.trimPastedNewline()
	}
	m.updateCandidates()
	m.resize()
	return m, cmd
}

func (m *Model) trimPastedNewline() {
	value := m.input.Value()
	if trimmed := strings.TrimRight(value, "\r\n"); trimmed != value {
		m.input.SetValue(trimmed)
	}
}
func (m *Model) updateCandidates() {
	s := strings.TrimSpace(m.input.Value())
	m.candidates = nil
	m.candidateIndex = 0
	if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \n") {
		return
	}
	for _, name := range commandNames {
		if strings.HasPrefix(name, s) {
			m.candidates = append(m.candidates, name)
		}
	}
}
func (m Model) checkDetails() string {
	var b strings.Builder
	if len(m.memories) > 0 {
		b.WriteString("Historical references (not current evidence)\n\n")
		for _, h := range m.memories {
			when := "Time not recorded"
			if !h.RecordedAt.IsZero() {
				when = h.RecordedAt.Local().Format("2006-01-02 15:04")
			}
			b.WriteString("Source run " + safe(h.SourceRunID) + " · " + when + " · Conclusion status " + safe(h.ConclusionStatus) + "\n" + clip(safe(h.Content), 4096) + "\n\n")
		}
	}
	if len(m.checks) > 0 {
		b.WriteString("Current tool checks\n\n")
	}
	for _, c := range m.checks {
		status := "Running"
		if c.done {
			status = "Completed"
		}
		b.WriteString(status + " · " + safe(c.tool) + " · " + safe(c.key) + "\n" + safe(c.summary))
		if c.err != "" {
			b.WriteString("\nError: " + safe(c.err))
		}
		b.WriteString("\n\n")
	}
	if b.Len() == 0 {
		return "No tool checks or historical references yet."
	}
	return b.String()
}
