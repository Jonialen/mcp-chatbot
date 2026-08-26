// Command host is the console chatbot that acts as an MCP host.
//
// At this stage it is a protocol smoke test: it launches the official
// filesystem MCP server, completes the handshake, lists the server's tools and
// invokes one, printing every JSON-RPC frame that crosses the transport.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/mcp"
	"github.com/Jonialen/mcp-chatbot/internal/mcplog"
	"github.com/Jonialen/mcp-chatbot/internal/transport"
)

const (
	serverLabel   = "filesystem"
	handshakeWait = 60 * time.Second
)

func main() {
	dir := flag.String("dir", ".", "directory to expose through the filesystem server")
	tool := flag.String("tool", "list_allowed_directories", "tool to invoke as a smoke test")
	args := flag.String("args", "{}", "arguments for the tool, as a JSON object")
	flag.Parse()

	if err := run(*dir, *tool, *args); err != nil {
		fmt.Fprintf(os.Stderr, "\nerror: %v\n", err)
		os.Exit(1)
	}
}

func run(dir, tool, rawArgs string) error {
	var toolArgs map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &toolArgs); err != nil {
		return fmt.Errorf("parse -args as a JSON object: %w", err)
	}

	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	if dir != "" {
		root = dir
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := mcplog.New(os.Stdout)

	// The filesystem server is an npm package, so it is launched through npx.
	// Nothing about this host is aware of that: it starts a process and speaks
	// JSON-RPC to it, which is the whole point of the protocol.
	tr, err := transport.NewStdio(transport.StdioConfig{
		Command:  "npx",
		Args:     []string{"-y", "@modelcontextprotocol/server-filesystem", root},
		OnStderr: func(line string) { log.Event(serverLabel, "stderr: "+line) },
		OnNotice: func(line string) { log.Event(serverLabel, line) },
	})
	if err != nil {
		return err
	}

	session := mcp.NewSession(ctx, mcp.SessionConfig{
		Name:      serverLabel,
		Transport: tr,
		LogFrame:  log.For(serverLabel),
		OnNotification: func(method string, _ json.RawMessage) {
			log.Event(serverLabel, "notification: "+method)
		},
	})
	defer session.Close()

	// npx may need to download the package on a cold cache, so the handshake
	// gets a generous deadline of its own.
	handshakeCtx, cancel := context.WithTimeout(ctx, handshakeWait)
	defer cancel()

	info, err := session.Initialize(handshakeCtx)
	if err != nil {
		return err
	}

	fmt.Printf("\nconnected to %s %s (protocol %s, tools capability: %t)\n\n",
		info.ServerInfo.Name, info.ServerInfo.Version,
		session.ProtocolVersion(), info.SupportsTools())

	tools, err := session.ListTools(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("\n%d tools exposed:\n", len(tools))
	for _, t := range tools {
		fmt.Printf("  - %-32s %s\n", t.Name, firstLine(t.Description))
	}

	fmt.Printf("\ncalling %q...\n\n", tool)
	result, err := session.CallTool(ctx, tool, toolArgs)
	if err != nil {
		return err
	}

	// A tool that ran and failed is reported here, not raised: the flag travels
	// in the result so a model can read the failure and react to it.
	if result.IsError {
		fmt.Printf("\ntool reported an error:\n%s\n", result.Text())
		return nil
	}
	fmt.Printf("\ntool result:\n%s\n", result.Text())
	return nil
}

func firstLine(s string) string {
	const max = 72
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
