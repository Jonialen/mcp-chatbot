package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Jonialen/mcp-chatbot/internal/agent"
	"github.com/Jonialen/mcp-chatbot/internal/llm"
	"github.com/Jonialen/mcp-chatbot/internal/mcplog"
	"github.com/Jonialen/mcp-chatbot/internal/registry"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type fakeTUIBot struct {
	ask func(context.Context, string) (string, error)
}

func (b fakeTUIBot) Ask(ctx context.Context, input string) (string, error) { return b.ask(ctx, input) }
func (b fakeTUIBot) Usage() llm.Usage                                      { return llm.Usage{InputTokens: 12, OutputTokens: 3} }

func testTUI(t *testing.T, bot tuiBot) *tuiModel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newTUI(ctx, cancel, bot, &tuiFeed{}, "test-model", func(io.Writer, string) error { return nil })
}

func submitTUI(m *tuiModel, text string) tea.Cmd {
	m.input.SetValue(text)
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return cmd
}

func TestTUIAskLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"answer", nil, "bot > Hello"},
		{"failure", errors.New("provider unavailable"), "Error: provider unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			m := testTUI(t, fakeTUIBot{ask: func(ctx context.Context, input string) (string, error) {
				calls++
				if input != "hello" || ctx.Err() != nil {
					t.Fatalf("unexpected request: %q, %v", input, ctx.Err())
				}
				return "Hello", tc.err
			}})
			if submitTUI(m, "  ") != nil {
				t.Fatal("blank input scheduled work")
			}
			cmd := submitTUI(m, " hello ")
			if cmd == nil || !m.busy || calls != 0 {
				t.Fatal("request must be asynchronous")
			}
			if submitTUI(m, "/reset") != nil || m.input.Value() != "/reset" {
				t.Fatal("busy turn accepted a command")
			}
			m.Update(cmd())
			if calls != 1 || m.busy || !strings.Contains(m.transcript, tc.want) {
				t.Fatalf("unexpected result: %s", m.transcript)
			}
			if !strings.Contains(m.View(), "12 in / 3 out") {
				t.Fatal("usage missing")
			}
		})
	}
}

func TestTUIReusesCommands(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"/help", "/tools [server]"}, {"/tools absent", "no tools match"},
		{"/usage", "0 tokens in"}, {"/log", "whole frames"},
		{"/reset", "conversation cleared"}, {"/unknown", "unknown command"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			bot := agent.New(agent.Config{})
			reg := registry.New()
			log := mcplog.New(mcplog.Options{})
			m := testTUI(t, bot)
			m.command = func(out io.Writer, input string) error { return command(out, input, bot, reg, log) }
			m.append("old conversation\n")
			if cmd := submitTUI(m, tc.input); cmd != nil {
				t.Fatal("local command scheduled a model request")
			}
			if !strings.Contains(m.transcript, tc.want) {
				t.Fatalf("missing %q: %s", tc.want, m.transcript)
			}
			if tc.input == "/reset" && strings.Contains(m.transcript, "old conversation") {
				t.Fatal("reset kept transcript")
			}
			if tc.input == "/log" && !log.Verbose() {
				t.Fatal("log toggle not applied")
			}
		})
	}
}

func TestTUIQuitCancelsRequest(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan tea.Msg, 1)
	m := testTUI(t, fakeTUIBot{ask: func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	cmd := submitTUI(m, "wait")
	go func() { finished <- cmd() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request never started")
	}
	_, quit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if _, ok := quit().(tea.QuitMsg); !ok {
		t.Fatal("quit command missing")
	}
	select {
	case msg := <-finished:
		if !errors.Is(msg.(tuiAnswer).err, context.Canceled) {
			t.Fatal("request not canceled")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not stop")
	}
}

func TestTUIScrollResizeAndFeed(t *testing.T) {
	m := testTUI(t, fakeTUIBot{})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.append(strings.Repeat("long line with Unicode café 世界 and more text\n", 30))
	m.view.GotoBottom()
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.view.AtBottom() {
		t.Fatal("page up did not scroll")
	}
	offset := m.view.YOffset
	fmt.Fprintln(m.feed, "tool completed")
	m.Update(tuiTick(time.Now()))
	if !strings.Contains(m.logs, "tool completed") || strings.Contains(m.transcript, "tool completed") || m.view.YOffset != offset {
		t.Fatal("feed lost output or interrupted scroll")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if !m.view.AtBottom() {
		t.Fatal("jump to bottom failed")
	}
	m.Update(tea.WindowSizeMsg{Width: 10, Height: 3})
	if !strings.Contains(m.View(), "Resize terminal") {
		t.Fatal("small terminal hint missing")
	}
}

func TestTUIFeedBoundedAndSafe(t *testing.T) {
	feed := &tuiFeed{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); fmt.Fprint(feed, strings.Repeat("é", tuiTextLimit)) }()
	}
	wg.Wait()
	text := feed.drain()
	if !utf8.ValidString(text) || len([]rune(text)) != tuiTextLimit || feed.drain() != "" {
		t.Fatal("feed bound/drain failed")
	}
	for _, control := range []string{"\x1b[2J", "\r", "\a", "\u009b2J", "\u202e"} {
		if got := safeText(control); strings.ContainsAny(got, "\x1b\r\a\u009b\u202e") {
			t.Fatalf("unsafe output: %q", got)
		}
	}
}

func TestTUIProgramLocalSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bot := agent.New(agent.Config{})
	reg := registry.New()
	log := mcplog.New(mcplog.Options{})
	m := newTUI(ctx, func() {}, bot, &tuiFeed{}, "offline", func(out io.Writer, input string) error {
		return command(out, input, bot, reg, log)
	})
	// Exercise the actual event loop and keyboard parser without a TTY, peers,
	// credentials, or a live model. Commands stay on the local host boundary.
	var screen bytes.Buffer
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(strings.NewReader("/help\r/quit\r")),
		tea.WithOutput(&screen), tea.WithAltScreen(), tea.WithoutSignalHandler())
	if _, err := p.Run(); err != nil {
		t.Fatalf("program: %v", err)
	}
	if !strings.Contains(m.transcript, "/tools [server]") {
		t.Fatal("keyboard session did not execute /help")
	}
	if !strings.Contains(screen.String(), "MCP Chatbot") {
		t.Fatal("terminal renderer did not display the chat")
	}
}

func TestTUIReceivesBackgroundActivity(t *testing.T) {
	m := testTUI(t, fakeTUIBot{})
	log := mcplog.New(mcplog.Options{Screen: m.feed})
	log.Event("local", "server notice")
	progressReporter(m.feed)(agent.Event{Kind: agent.ToolStarted, Tool: "local__lookup"})
	m.Update(tuiTick(time.Now()))
	for _, want := range []string{"server notice", "local__lookup"} {
		if !strings.Contains(m.logs, want) {
			t.Fatalf("missing activity %q", want)
		}
	}
}

func TestCLICommandsRemainAvailable(t *testing.T) {
	bot := agent.New(agent.Config{})
	var out bytes.Buffer
	err := repl(context.Background(), bufio.NewScanner(strings.NewReader("/usage\n/reset\n/quit\n")),
		&out, bot, registry.New(), mcplog.New(mcplog.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"you >", "0 tokens in", "conversation cleared"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("CLI missing %q: %s", want, &out)
		}
	}
}

func TestTUICancelKeepsSessionAndSerializesTurns(t *testing.T) {
	for _, cooperative := range []bool{true, false} {
		t.Run(fmt.Sprintf("cooperative=%v", cooperative), func(t *testing.T) {
			var request context.Context
			m := testTUI(t, fakeTUIBot{ask: func(ctx context.Context, _ string) (string, error) {
				request = ctx
				if cooperative {
					return "", ctx.Err()
				}
				return "Completed before cancellation took effect", nil
			}})
			cmd := submitTUI(m, "wait")
			m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			if !m.busy || !m.cancelling || m.ctx.Err() != nil || submitTUI(m, "overlap") != nil {
				t.Fatal("cancel must keep session alive and block overlapping turns until completion")
			}
			m.Update(cmd())
			if request.Err() != context.Canceled || m.busy || m.cancelling || m.ctx.Err() != nil {
				t.Fatal("cancel completion did not restore the session")
			}
			want := "Request canceled"
			if !cooperative {
				want = "Completed before cancellation took effect"
			}
			if !strings.Contains(m.transcript, want) {
				t.Fatal(m.transcript)
			}
			if submitTUI(m, "next") == nil {
				t.Fatal("next request disabled")
			}
		})
	}
}

func TestTUILogsNavigationUnderLoad(t *testing.T) {
	m := testTUI(t, fakeTUIBot{})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	submitTUI(m, "waiting") // Do not execute the synthetic provider command.
	fmt.Fprint(m.feed, strings.Repeat("synthetic frame café 世界\n", 100))
	m.Update(tuiTick(m.started.Add(2 * time.Second)))
	if !m.unread || !m.logView.AtBottom() || !strings.Contains(m.View(), "Working 2s") {
		t.Fatal("live status missing")
	}
	first := m.View()
	m.Update(tuiTick(m.started.Add(2100 * time.Millisecond)))
	if first == m.View() {
		t.Fatal("working indicator did not animate")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.unread || !strings.Contains(m.View(), "synthetic frame") {
		t.Fatal("logs not accessible while busy")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	offset := m.logView.YOffset
	fmt.Fprintln(m.feed, "latest frame")
	m.Update(tuiTick(time.Now()))
	if m.logView.YOffset != offset || !m.unread {
		t.Fatal("new logs interrupted reading")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnd})
	if !m.logView.AtBottom() || m.unread {
		t.Fatal("tail did not resume")
	}
	for _, size := range []tea.WindowSizeMsg{{Width: 30, Height: 10}, {Width: 120, Height: 40}, {Width: 45, Height: 14}} {
		m.Update(size)
		if !m.logView.AtBottom() {
			t.Fatal("resize lost tail")
		}
		if lipgloss.Height(m.View()) > size.Height || lipgloss.Width(m.View()) > size.Width {
			t.Fatalf("layout exceeds %dx%d: %dx%d", size.Width, size.Height, lipgloss.Width(m.View()), lipgloss.Height(m.View()))
		}
	}
	fmt.Fprint(m.feed, strings.Repeat("é", tuiTextLimit+100))
	m.Update(tuiTick(time.Now()))
	if len([]rune(m.logs)) > tuiTextLimit || !utf8.ValidString(m.logs) {
		t.Fatal("logs not bounded")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.showLogs || !m.input.Focused() || strings.Contains(m.transcript, "synthetic frame") {
		t.Fatal("chat/log separation failed")
	}
}

// Observe only on the event-loop goroutine; the test never reads a running model.
type observedTUI struct {
	*tuiModel
	states chan string
}

func (m *observedTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.tuiModel.Update(msg)
	state := ""
	if m.busy && m.showLogs && strings.Contains(m.logs, "synthetic wait") && m.frame > 0 {
		state = "live"
	}
	if _, ok := msg.(tuiAnswer); ok && !m.busy {
		state = "complete"
	}
	if state != "" {
		select {
		case m.states <- state:
		default:
		}
	}
	return m, cmd
}

func TestTUIProgramInFlightLogsAndCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	feed := &tuiFeed{}
	bot := fakeTUIBot{ask: func(ctx context.Context, _ string) (string, error) {
		fmt.Fprintln(feed, "synthetic wait")
		<-ctx.Done()
		return "", ctx.Err()
	}}
	m := &observedTUI{newTUI(ctx, cancel, bot, feed, "offline", nil), make(chan string, 32)}
	var screen bytes.Buffer
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(&screen), tea.WithoutSignalHandler())
	finished := make(chan error, 1)
	go func() { _, err := p.Run(); finished <- err }()
	p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("wait")})
	p.Send(tea.KeyMsg{Type: tea.KeyEnter})
	p.Send(tea.KeyMsg{Type: tea.KeyTab})
	waitState := func(want string) {
		t.Helper()
		for {
			select {
			case state := <-m.states:
				if state == want {
					return
				}
			case <-ctx.Done():
				t.Fatalf("event loop did not reach %s", want)
			}
		}
	}
	waitState("live")
	p.Send(tea.KeyMsg{Type: tea.KeyPgUp})
	p.Send(tea.WindowSizeMsg{Width: 45, Height: 14})
	p.Send(tea.KeyMsg{Type: tea.KeyEsc})
	waitState("complete")
	p.Send(tea.QuitMsg{})
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.transcript, "Request canceled") || ctx.Err() != nil {
		t.Fatal("cancel exited session")
	}
	if !strings.Contains(screen.String(), "MCP Chatbot") {
		t.Fatal("renderer was not exercised")
	}
}

func TestTUIShutdownRestoresAlternateScreen(t *testing.T) {
	for _, mode := range []string{"ctrl+c", "ctrl+d", "parent canceled", "already canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started, stopped := make(chan struct{}), make(chan struct{})
			feed := &tuiFeed{}
			bot := fakeTUIBot{ask: func(ctx context.Context, _ string) (string, error) {
				close(started)
				defer close(stopped)
				for {
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					default:
						fmt.Fprintln(feed, "synthetic frame during shutdown")
					}
				}
			}}
			m := newTUI(ctx, cancel, bot, feed, "offline", nil)
			var screen bytes.Buffer
			input, keyboard, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer keyboard.Close()
			if mode == "already canceled" {
				cancel()
			}
			finished := make(chan error, 1)
			go func() {
				_, err := runTerminalProgram(ctx, m, tea.WithInput(input), tea.WithOutput(&screen))
				finished <- err
			}()
			if mode != "already canceled" {
				fmt.Fprint(keyboard, "wait\r\t")
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("request did not start")
				}
				switch mode {
				case "ctrl+c":
					fmt.Fprint(keyboard, "\x03")
				case "ctrl+d":
					fmt.Fprint(keyboard, "\x04")
				default:
					cancel()
				}
			}
			select {
			case err := <-finished:
				if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				stack := make([]byte, 32000)
				t.Fatalf("event loop did not return after cancellation\n%s", stack[:runtime.Stack(stack, true)])
			}
			if mode != "already canceled" {
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("request survived shutdown")
				}
			}
			output := screen.String()
			entered, exited := strings.LastIndex(output, "\x1b[?1049h"), strings.LastIndex(output, "\x1b[?1049l")
			if entered >= 0 && exited <= entered {
				t.Fatal("alternate screen was not restored")
			}
			if mode != "already canceled" && entered < 0 {
				t.Fatal("test never entered alternate screen")
			}
			if mode != "already canceled" && !strings.Contains(output, "\x1b[?25h") {
				t.Fatal("cursor not restored")
			}
		})
	}
}
