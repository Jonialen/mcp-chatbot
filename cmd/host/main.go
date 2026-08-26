// Command host is a console chatbot that acts as an MCP host.
//
// It launches the MCP servers named in a configuration file, collects their
// tools into one list, hands that list to a model, and runs the loop in which
// the model asks for a tool and this program carries the request out.
//
// The Model Context Protocol is implemented directly over JSON-RPC 2.0, with no
// MCP SDK: every frame in the log was built and parsed by this codebase.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/agent"
	"github.com/Jonialen/mcp-chatbot/internal/config"
	"github.com/Jonialen/mcp-chatbot/internal/llm/gemini"
	"github.com/Jonialen/mcp-chatbot/internal/mcplog"
	"github.com/Jonialen/mcp-chatbot/internal/registry"
)

func main() {
	opts := parseFlags()

	if err := run(opts); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	configPath string
	logDir     string
	model      string
	verbose    bool
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.configPath, "config", "config/servers.json", "MCP server configuration")
	flag.StringVar(&opts.logDir, "logs", "logs", "directory for the JSON-RPC frame log")
	flag.StringVar(&opts.model, "model", "", "model to use (default "+gemini.DefaultModel+")")
	flag.BoolVar(&opts.verbose, "verbose", false, "show whole JSON-RPC frames on screen")
	flag.Parse()
	return opts
}

func run(opts options) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	logFile, logPath, err := openLog(opts.logDir)
	if err != nil {
		return err
	}
	defer logFile.Close()

	log := mcplog.New(mcplog.Options{
		File:    logFile,
		Screen:  os.Stdout,
		Verbose: opts.verbose,
	})

	provider, err := gemini.New(ctx, gemini.Config{Model: opts.model})
	if err != nil {
		return err
	}

	reg := registry.New()
	defer reg.Close()

	fmt.Printf("connecting to %d MCP servers...\n\n", len(cfg.Enabled()))

	started := time.Now()
	connections := connectAll(ctx, cfg, reg, log)
	elapsed := time.Since(started)

	banner(provider.Name(), provider.Model(), logPath, connections, reg, elapsed)

	if reg.Len() == 0 {
		return fmt.Errorf("no MCP server came up; nothing to demonstrate")
	}

	bot := agent.New(agent.Config{
		Provider: provider,
		Tools:    reg,
		Observe:  progressReporter(os.Stdout),
	})

	return repl(ctx, bufio.NewScanner(os.Stdin), os.Stdout, bot, reg, log)
}

// openLog creates the file the frame record is written to. Every run gets its
// own file, so a demonstration can be replayed from the exact session it came
// from.
func openLog(dir string) (*os.File, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create log directory: %w", err)
	}

	path := filepath.Join(dir, fmt.Sprintf("mcp-%s.log", time.Now().Format("20060102-150405")))
	file, err := os.Create(path)
	if err != nil {
		return nil, "", fmt.Errorf("create log file: %w", err)
	}
	return file, path, nil
}

func banner(
	vendor, model, logPath string,
	connections []connection,
	reg *registry.Registry,
	elapsed time.Duration,
) {
	fmt.Println()
	for _, c := range connections {
		fmt.Println(describe(c, elapsed))
	}

	fmt.Printf("\n  model         %s (%s)\n", model, vendor)
	fmt.Printf("  tools         %d\n", reg.Len())
	fmt.Printf("  frame log     %s\n", logPath)
	fmt.Printf("\ntype /help for commands, /quit to leave\n\n")
}
