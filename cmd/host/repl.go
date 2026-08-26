package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/agent"
	"github.com/Jonialen/mcp-chatbot/internal/mcplog"
	"github.com/Jonialen/mcp-chatbot/internal/registry"
)

// errQuit ends the session cleanly.
var errQuit = errors.New("quit")

// repl reads questions and prints answers until the user leaves or input ends.
func repl(
	ctx context.Context,
	in *bufio.Scanner,
	out io.Writer,
	bot *agent.Agent,
	reg *registry.Registry,
	log *mcplog.Logger,
) error {
	for {
		fmt.Fprint(out, "you > ")

		if !in.Scan() {
			fmt.Fprintln(out)
			return in.Err()
		}

		input := strings.TrimSpace(in.Text())
		if input == "" {
			continue
		}

		if strings.HasPrefix(input, "/") {
			if err := command(out, input, bot, reg, log); err != nil {
				if errors.Is(err, errQuit) {
					printUsage(out, bot)
					return nil
				}
				fmt.Fprintf(out, "  %v\n\n", err)
			}
			continue
		}

		answer, err := bot.Ask(ctx, input)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintf(out, "\n  %v\n\n", err)
			continue
		}
		fmt.Fprintf(out, "\nbot > %s\n\n", answer)
	}
}

func command(
	out io.Writer,
	input string,
	bot *agent.Agent,
	reg *registry.Registry,
	log *mcplog.Logger,
) error {
	fields := strings.Fields(input)

	switch fields[0] {
	case "/quit", "/exit":
		return errQuit

	case "/help":
		fmt.Fprint(out, `
  /tools [server]   list the tools available, optionally from one server
  /log              toggle whole JSON-RPC frames on screen
  /usage            tokens spent in this conversation
  /reset            forget the conversation, keep the servers connected
  /quit             leave

`)

	case "/tools":
		filter := ""
		if len(fields) > 1 {
			filter = fields[1]
		}
		listTools(out, reg, filter)

	case "/log":
		log.SetVerbose(!log.Verbose())
		state := "summaries"
		if log.Verbose() {
			state = "whole frames"
		}
		fmt.Fprintf(out, "  screen now shows %s; the file always holds everything\n\n", state)

	case "/usage":
		printUsage(out, bot)

	case "/reset":
		bot.Reset()
		fmt.Fprint(out, "  conversation cleared; servers still connected\n\n")

	default:
		return fmt.Errorf("unknown command %q; try /help", fields[0])
	}
	return nil
}

func listTools(out io.Writer, reg *registry.Registry, filter string) {
	defs := reg.Definitions()

	byServer := make(map[string][]string)
	for _, def := range defs {
		server, ok := reg.ServerOf(def.Name)
		if !ok {
			continue
		}
		if filter != "" && server != filter {
			continue
		}
		byServer[server] = append(byServer[server], def.Name)
	}

	if len(byServer) == 0 {
		fmt.Fprintf(out, "  no tools match %q\n\n", filter)
		return
	}

	names := make([]string, 0, len(byServer))
	for server := range byServer {
		names = append(names, server)
	}
	sort.Strings(names)

	fmt.Fprintln(out)
	for _, server := range names {
		fmt.Fprintf(out, "  %s (%d)\n", server, len(byServer[server]))
		for _, tool := range byServer[server] {
			fmt.Fprintf(out, "    %s\n", tool)
		}
	}
	fmt.Fprintln(out)
}

func printUsage(out io.Writer, bot *agent.Agent) {
	usage := bot.Usage()
	fmt.Fprintf(out, "  %d tokens in, %d tokens out\n\n", usage.InputTokens, usage.OutputTokens)
}

// progressReporter shows tool activity while a turn is in flight, so a slow
// answer reads as work being done rather than as a hang.
//
// Events arrive from the goroutines running the tools, so the writer is guarded.
func progressReporter(out io.Writer) agent.Observer {
	var mu sync.Mutex

	return func(event agent.Event) {
		mu.Lock()
		defer mu.Unlock()

		switch event.Kind {
		case agent.ToolStarted:
			fmt.Fprintf(out, "  ⚙ %s %s\n", event.Tool, compactArgs(event.Args))
		case agent.ToolFinished:
			mark := "✓"
			if event.IsError {
				mark = "✗"
			}
			fmt.Fprintf(out, "  %s %s (%s)\n", mark, event.Tool, event.Elapsed.Round(time.Millisecond))
		}
	}
}

// compactArgs renders a call's arguments on one line, short enough to read
// while the answer is still being produced.
func compactArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", key, args[key]))
	}

	line := strings.Join(parts, " ")
	const max = 100
	if len(line) > max {
		return line[:max] + "..."
	}
	return line
}
