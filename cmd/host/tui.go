package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Jonialen/mcp-chatbot/internal/agent"
	"github.com/Jonialen/mcp-chatbot/internal/llm"
	"github.com/Jonialen/mcp-chatbot/internal/mcplog"
	"github.com/Jonialen/mcp-chatbot/internal/registry"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

const tuiTextLimit = 64000

// tuiFeed keeps background logging off the terminal and never blocks a tool on
// the UI event loop. Only the display is bounded; the frame log stays complete.
type tuiFeed struct {
	mu   sync.Mutex
	text string
}

func (f *tuiFeed) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.text = lastRunes(f.text+safeText(string(p)), tuiTextLimit)
	return len(p), nil
}

func (f *tuiFeed) drain() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.text
	f.text = ""
	return s
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// Treat all model/server output as text, never terminal control sequences.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

type tuiBot interface {
	Ask(context.Context, string) (string, error)
	Usage() llm.Usage
}

type tuiTick time.Time
type tuiAnswer struct {
	text string
	err  error
}

type tuiModel struct {
	ctx           context.Context
	cancel        context.CancelFunc
	bot           tuiBot
	command       func(io.Writer, string) error
	feed          *tuiFeed
	input         textinput.Model
	view          viewport.Model
	logView       viewport.Model
	logs          string
	showLogs      bool
	unread        bool
	requestCancel context.CancelFunc
	cancelling    bool
	started       time.Time
	elapsed       time.Duration
	frame         int
	transcript    string
	model         string
	busy          bool
	height        int
}

func newTUI(ctx context.Context, cancel context.CancelFunc, bot tuiBot, feed *tuiFeed, name string, command func(io.Writer, string) error) *tuiModel {
	input := textinput.New()
	input.Prompt = "you > "
	input.Placeholder = "Ask a question or type /help"
	input.CharLimit = 4096
	input.Width = 72
	input.Focus()
	m := &tuiModel{ctx: ctx, cancel: cancel, bot: bot, command: command, feed: feed,
		input: input, view: viewport.New(78, 15), logView: viewport.New(78, 15), model: strings.ReplaceAll(safeText(name), "\n", " "), height: 23}
	m.append("Welcome. /tools lists connected tools; /help lists commands.\n\nTab opens live logs, including tool activity and provider wait notices.\n")
	m.drainLogs()
	return m
}

func runTUI(ctx context.Context, bot *agent.Agent, reg *registry.Registry, log *mcplog.Logger, feed *tuiFeed, name string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newTUI(ctx, cancel, bot, feed, name, func(out io.Writer, input string) error {
		return command(out, input, bot, reg, log)
	})
	_, err := runTerminalProgram(ctx, m)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func nextTUITick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg { return tuiTick(t) })
}

func (m *tuiModel) Init() tea.Cmd { return tea.Batch(textinput.Blink, nextTUITick()) }

func (m *tuiModel) append(s string) {
	follow := m.view.AtBottom()
	m.transcript = lastRunes(m.transcript+safeText(s), tuiTextLimit)
	m.renderTranscript()
	if follow {
		m.view.GotoBottom()
	}
}

func (m *tuiModel) renderTranscript() {
	m.view.SetContent(runewidth.Wrap(m.transcript, max(1, m.view.Width)))
}

func (m *tuiModel) drainLogs() {
	text := m.feed.drain()
	if text == "" {
		return
	}
	follow := m.logView.AtBottom()
	m.logs = lastRunes(m.logs+text, tuiTextLimit)
	m.logView.SetContent(runewidth.Wrap(m.logs, max(1, m.logView.Width)))
	if follow {
		m.logView.GotoBottom()
	}
	m.unread = !m.showLogs || !m.logView.AtBottom()
}

func (m *tuiModel) activeView() *viewport.Model {
	if m.showLogs {
		return &m.logView
	}
	return &m.view
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		chatTail, logTail := m.view.AtBottom(), m.logView.AtBottom()
		m.height = msg.Height
		m.view.Width = max(1, msg.Width-2)
		m.view.Height = max(1, msg.Height-8)
		m.logView.Width, m.logView.Height = m.view.Width, m.view.Height
		m.input.Width = max(1, msg.Width-8)
		m.renderTranscript()
		m.logView.SetContent(runewidth.Wrap(m.logs, m.logView.Width))
		if chatTail {
			m.view.GotoBottom()
		}
		if logTail {
			m.logView.GotoBottom()
		}
		return m, nil
	case tuiTick:
		m.drainLogs()
		if m.busy {
			m.elapsed = max(0, time.Time(msg).Sub(m.started))
			m.frame++
		}
		return m, nextTUITick()
	case tuiAnswer:
		if !m.busy {
			return m, nil
		}
		m.requestCancel()
		m.requestCancel = nil
		m.busy = false
		m.drainLogs()
		if errors.Is(msg.err, context.Canceled) {
			m.append("Request canceled. You can send another question.\n\n")
		} else if msg.err != nil {
			m.append("Error: " + msg.err.Error() + "\n")
		} else {
			m.append("bot > " + msg.text + "\n\n")
		}
		m.cancelling = false
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			m.cancel()
			return m, tea.Quit
		case "esc":
			if m.busy && !m.cancelling {
				m.cancelling = true
				m.requestCancel()
			}
			return m, nil
		case "tab", "shift+tab":
			m.showLogs = !m.showLogs
			if m.showLogs {
				m.input.Blur()
				if m.logView.AtBottom() {
					m.unread = false
				}
				return m, nil
			}
			return m, m.input.Focus()
		case "pgup", "pgdown", "ctrl+home", "ctrl+end":
			view := m.activeView()
			if msg.String() == "ctrl+home" {
				view.GotoTop()
				return m, nil
			}
			if msg.String() == "ctrl+end" {
				view.GotoBottom()
				if m.showLogs {
					m.unread = false
				}
				return m, nil
			}
			var cmd tea.Cmd
			*view, cmd = view.Update(msg)
			if m.showLogs && view.AtBottom() {
				m.unread = false
			}
			return m, cmd
		case "enter":
			if m.busy || m.showLogs {
				return m, nil
			}
			input := strings.TrimSpace(m.input.Value())
			if input == "" {
				return m, nil
			}
			m.input.SetValue("")
			m.append("you > " + input + "\n")
			if strings.HasPrefix(input, "/") {
				var out strings.Builder
				err := m.command(&out, input)
				if errors.Is(err, errQuit) {
					m.cancel()
					return m, tea.Quit
				}
				if err != nil {
					fmt.Fprintln(&out, err)
				}
				if strings.Fields(input)[0] == "/reset" {
					m.transcript = ""
				}
				m.append(out.String())
				if strings.Fields(input)[0] == "/help" {
					m.append("TUI: Tab chat/logs; PgUp/PgDn scroll; Ctrl+Home/End first/latest; Esc cancel request; Ctrl+C/D quit.\n")
				}
				if strings.Fields(input)[0] == "/log" {
					m.drainLogs()
					m.showLogs = true
					m.logView.GotoBottom()
					m.unread = false
					m.input.Blur()
				}
				return m, nil
			}
			m.busy = true
			m.started, m.elapsed, m.frame = time.Now(), 0, 0
			ctx, cancel := context.WithCancel(m.ctx)
			m.requestCancel = cancel
			bot := m.bot
			return m, func() tea.Msg {
				text, err := bot.Ask(ctx, input)
				return tuiAnswer{text, err}
			}
		}
	}
	if m.showLogs {
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		if m.logView.AtBottom() {
			m.unread = false
		}
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *tuiModel) View() string {
	if m.view.Width < 28 || m.height < 10 {
		return "Resize terminal (30x10 minimum)\nEsc cancel request; Ctrl+C quit"
	}
	usage := m.bot.Usage()
	state := "Ready"
	if m.busy {
		state = fmt.Sprintf("%s Working %s", []string{"|", "/", "-", "\\"}[m.frame%4], m.elapsed.Truncate(time.Second))
		if m.cancelling {
			state = fmt.Sprintf("Canceling %s", m.elapsed.Truncate(time.Second))
		}
	}
	width := m.view.Width + 2
	clip := func(s string) string { return runewidth.Truncate(s, width, "...") }
	header := tuiAccent.Render(clip("MCP Chatbot / " + m.model))
	status := clip(fmt.Sprintf("%s | tokens: %d in / %d out", state, usage.InputTokens, usage.OutputTokens))
	tabs := "[Chat]   Logs"
	input := m.input.View()
	if m.showLogs {
		tabs = "Chat   [Logs]"
		input = tuiMuted.Render(clip("Live activity / /log toggles full frames in Chat"))
	}
	if m.unread {
		tabs += " * new"
	}
	position := "tail"
	if !m.activeView().AtBottom() {
		position = "scrolled"
	}
	tabs += " | " + position
	panel := tuiBorder.Width(m.view.Width).Render(m.activeView().View())
	controls := "Tab chat/logs | Esc cancel | Ctrl+C quit"
	navigation := "PgUp/Dn scroll | Ctrl+Home/End | Enter send | /help"
	if width < 50 {
		controls = "Tab logs | Esc cancel | ^C quit"
		navigation = "PgUp/Dn scroll | /help"
		if m.showLogs {
			controls = "Tab chat | Esc cancel | ^C quit"
		}
	}
	return strings.Join([]string{header, status, tuiAccent.Render(clip(tabs)), panel, input,
		tuiMuted.Render(clip(controls)),
		tuiMuted.Render(clip(navigation))}, "\n")
}

var (
	tuiAccent = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("110"))
	tuiMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	tuiBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("60"))
)
