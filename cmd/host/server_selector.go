package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Jonialen/mcp-chatbot/internal/config"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

type serverSelector struct {
	cfg           *config.Config
	names         []string
	selected      map[string]bool
	cursor        int
	width, height int
	confirmed     bool
	notice        string
}

func newServerSelector(cfg *config.Config) *serverSelector {
	m := &serverSelector{cfg: cfg, selected: make(map[string]bool), width: 80, height: 24}
	for name, server := range cfg.Servers {
		m.names = append(m.names, name)
		m.selected[name] = !server.Disabled
	}
	sort.Strings(m.names)
	return m
}

func selectServers(ctx context.Context, cfg *config.Config) (*config.Config, error) {
	return runServerSelector(ctx, cfg)
}

func runServerSelector(ctx context.Context, cfg *config.Config, opts ...tea.ProgramOption) (*config.Config, error) {
	m := newServerSelector(cfg)
	_, err := runTerminalProgram(ctx, m, opts...)
	if ctx.Err() != nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("server selection: %w", err)
	}
	if !m.confirmed {
		return nil, nil
	}
	return m.selection(), nil
}

// Copy only selected entries; disabled entries can be explicitly enabled for
// this session without changing the loaded configuration or its file.
func (m *serverSelector) selection() *config.Config {
	cfg := &config.Config{Servers: make(map[string]config.Server)}
	for _, name := range m.names {
		if m.selected[name] {
			server := m.cfg.Servers[name]
			server.Disabled = false
			cfg.Servers[name] = server
		}
	}
	return cfg
}

func (m *serverSelector) Init() tea.Cmd { return nil }

func (m *serverSelector) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.confirmed = false
			return m, tea.Quit
		case "up":
			m.cursor = max(0, m.cursor-1)
		case "down":
			m.cursor = max(0, min(len(m.names)-1, m.cursor+1))
		case " ":
			if len(m.names) > 0 {
				name := m.names[m.cursor]
				m.selected[name] = !m.selected[name]
				m.notice = ""
			}
		case "enter":
			if len(m.selection().Servers) == 0 {
				m.notice = "Select at least one server, or Esc to cancel."
				return m, nil
			}
			m.confirmed = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *serverSelector) View() string {
	if m.width < 30 || m.height < 10 {
		return "Resize terminal (30x10 minimum)\nEsc / Ctrl+C cancel"
	}
	lines := []string{"Select MCP servers | nothing connected yet",
		"Enabled entries start checked; disabled entries can be opted in.",
		"Session only: configuration is not saved."}
	rows := max(1, m.height-9)
	start := max(0, m.cursor-rows+1)
	for i := start; i < min(len(m.names), start+rows); i++ {
		name := m.names[i]
		cursor, checked := " ", " "
		if i == m.cursor {
			cursor = ">"
		}
		if m.selected[name] {
			checked = "x"
		}
		server := m.cfg.Servers[name]
		kind := "stdio"
		if server.IsRemote() {
			kind = "HTTP"
		}
		if server.Disabled {
			kind += "; disabled by default"
		}
		lines = append(lines, fmt.Sprintf("%s [%s] %s (%s)", cursor, checked, safeText(name), kind))
	}
	lines = append(lines, fmt.Sprintf("%d selected | row %d/%d", len(m.selection().Servers), min(m.cursor+1, len(m.names)), len(m.names)), m.notice,
		"Up/Down move | Space toggle", "Enter connect | Esc / Ctrl+C cancel")
	for i := range lines {
		lines[i] = runewidth.Truncate(lines[i], m.width-2, "...")
	}
	lines[0] = tuiAccent.Render(lines[0])
	return tuiBorder.Width(m.width - 2).Render(strings.Join(lines, "\n"))
}
