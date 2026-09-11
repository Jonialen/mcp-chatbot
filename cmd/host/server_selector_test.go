package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/config"
	tea "github.com/charmbracelet/bubbletea"
)

func selectorConfig() *config.Config {
	return &config.Config{Servers: map[string]config.Server{
		"alpha":  {Command: "never-start-this-command", Args: []string{"arg"}, Env: []string{"KEY=value"}, Dir: "/unused"},
		"hotel":  {Command: "never-start-hotel", Disabled: true},
		"remote": {URL: "https://example.invalid/mcp", Headers: map[string]string{"Authorization": "not-a-secret"}},
	}}
}

func TestServerSelectorSelection(t *testing.T) {
	cfg := selectorConfig()
	m := newServerSelector(cfg)
	if !reflect.DeepEqual(m.names, []string{"alpha", "hotel", "remote"}) || m.selected["hotel"] || !m.selected["alpha"] || !m.selected["remote"] {
		t.Fatalf("unexpected defaults: %+v", m)
	}
	for _, key := range []tea.KeyType{tea.KeyUp, tea.KeySpace, tea.KeyDown, tea.KeySpace, tea.KeyDown, tea.KeyDown, tea.KeySpace} {
		m.Update(tea.KeyMsg{Type: key})
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !m.confirmed {
		t.Fatal("selection did not finish")
	}
	selected := m.selection()
	if len(selected.Enabled()) != 1 || selected.Enabled()[0].Name != "hotel" {
		t.Fatalf("selection = %+v", selected)
	}
	if !reflect.DeepEqual(cfg, selectorConfig()) {
		t.Fatal("source config was mutated")
	}
	defaults := newServerSelector(cfg).selection()
	if !reflect.DeepEqual(defaults.Servers["alpha"], cfg.Servers["alpha"]) || !reflect.DeepEqual(defaults.Servers["remote"], cfg.Servers["remote"]) {
		t.Fatal("connection settings were lost")
	}
}

func TestServerSelectorEmptyAndCancel(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			m := newServerSelector(&config.Config{Servers: map[string]config.Server{"hotel": {Command: "unused", Disabled: true}}})
			_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
			if cmd != nil || m.confirmed || !strings.Contains(m.View(), "Select at least one") {
				t.Fatal("empty selection was not rejected")
			}
			_, cmd = m.Update(tea.KeyMsg{Type: key})
			if cmd == nil || m.confirmed {
				t.Fatal("cancel did not quit")
			}
		})
	}
	m := newServerSelector(&config.Config{})
	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		t.Fatal("empty list accepted")
	}
}

func TestServerSelectorView(t *testing.T) {
	m := newServerSelector(selectorConfig())
	for _, text := range []string{"Space toggle", "Enter connect", "Ctrl+C cancel", "disabled by default", "nothing connected"} {
		if !strings.Contains(m.View(), text) {
			t.Errorf("missing help: %s", text)
		}
	}
	if strings.Contains(m.View(), "not-a-secret") {
		t.Fatal("headers exposed")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if !strings.Contains(m.View(), "> [x] remote") || len(strings.Split(m.View(), "\n")) > 10 {
		t.Fatal("cursor not visible in short terminal")
	}
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 4})
	if !strings.Contains(m.View(), "Resize") {
		t.Fatal("missing resize notice")
	}
}

func TestServerSelectorProgram(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		cancel      bool
	}{
		{"confirm defaults", "\r", false},
		{"escape", "\x1b", true},
		{"interrupt", "\x03", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cfg, err := runServerSelector(ctx, selectorConfig(), tea.WithInput(strings.NewReader(tt.input)), tea.WithOutput(io.Discard), tea.WithoutRenderer())
			if err != nil || ctx.Err() != nil {
				t.Fatalf("program: %v, context: %v", err, ctx.Err())
			}
			if (cfg == nil) != tt.cancel {
				t.Fatalf("cancel = %v, config = %+v", tt.cancel, cfg)
			}
			if cfg != nil && len(cfg.Enabled()) != 2 {
				t.Fatal("default selection lost")
			}
		})
	}
}

func TestRunSelectionBeforeStartup(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	for _, scenario := range []string{"cancel", "error", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "servers.json")
			raw := []byte(`{"mcpServers":{"test":{"command":"must-not-execute"}}}`)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			wantErr := errors.New("selector failed")
			called := false
			err := runWithSelector(options{tui: true, configPath: path, logDir: filepath.Join(dir, "logs")}, func(_ context.Context, cfg *config.Config) (*config.Config, error) {
				called = true
				if len(cfg.Enabled()) != 1 {
					t.Fatal("config not loaded first")
				}
				switch scenario {
				case "error":
					return nil, wantErr
				case "empty":
					return &config.Config{}, nil
				default:
					return nil, nil
				}
			})
			if !called {
				t.Fatal("selector not called")
			}
			if scenario == "cancel" && err != nil {
				t.Fatal(err)
			}
			if scenario == "error" && !errors.Is(err, wantErr) {
				t.Fatalf("error = %v", err)
			}
			if scenario == "empty" && (err == nil || !strings.Contains(err.Error(), "select at least one")) {
				t.Fatalf("error = %v", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "logs")); !os.IsNotExist(err) {
				t.Fatal("logging initialized before selection")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) {
				t.Fatal("config file changed")
			}
		})
	}
}

func TestCLISkipsSelection(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"test":{"command":"must-not-execute"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := runWithSelector(options{configPath: path, logDir: filepath.Join(dir, "logs")}, func(context.Context, *config.Config) (*config.Config, error) {
		t.Fatal("CLI called selector")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("expected existing CLI credential check, got %v", err)
	}
}
