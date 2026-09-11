package main

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// Keep the renderer alive until it consumes Quit and restores the terminal.
// Bubble Tea v1.3.4 sends commands without a cancellation select; canceling its
// context during Update can stop the command consumer before that send finishes.
// The host owns OS signals through signal.NotifyContext. Bridge cancellation to
// Quit instead, including signals received while the selector is open.
func runTerminalProgram(ctx context.Context, model tea.Model, opts ...tea.ProgramOption) (tea.Model, error) {
	if ctx.Err() != nil {
		return model, nil
	}
	opts = append(opts, tea.WithAltScreen(), tea.WithoutSignalHandler(), tea.WithContext(context.WithoutCancel(ctx)))
	p := tea.NewProgram(model, opts...)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.Quit()
		case <-done:
		}
	}()
	return p.Run()
}
