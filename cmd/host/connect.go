package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/config"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
	"github.com/Jonialen/mcp-chatbot/internal/mcplog"
	"github.com/Jonialen/mcp-chatbot/internal/registry"
	"github.com/Jonialen/mcp-chatbot/internal/transport"
)

// connection reports what happened when one configured server was contacted.
type connection struct {
	Name      string
	Transport string
	Server    mcp.Implementation
	Version   string
	Tools     []string
	Err       error
}

// OK reports whether the server is usable.
func (c connection) OK() bool { return c.Err == nil }

// connectAll brings up every enabled server and registers its tools.
//
// One server's failure is recorded and stepped over rather than returned. Two
// of the servers this host is required to run are written by other people, and
// a host that refuses to start because one of them is broken is a host that
// cannot demonstrate the other five.
func connectAll(
	ctx context.Context,
	cfg *config.Config,
	reg *registry.Registry,
	log *mcplog.Logger,
) []connection {
	servers := cfg.Enabled()
	results := make([]connection, 0, len(servers))

	for _, entry := range servers {
		results = append(results, connectOne(ctx, entry, reg, log))
	}
	return results
}

func connectOne(
	ctx context.Context,
	entry config.NamedServer,
	reg *registry.Registry,
	log *mcplog.Logger,
) connection {
	result := connection{Name: entry.Name, Transport: "stdio"}

	// The two transports differ in everything except what they carry, which is
	// why the choice ends here: the session, the JSON-RPC client and the
	// registry above never learn whether this server is a child process on this
	// machine or a service on the far side of the internet.
	var (
		tr  transport.Transport
		err error
	)

	if entry.IsRemote() {
		result.Transport = "http"
		tr, err = transport.NewHTTP(transport.HTTPConfig{
			Endpoint: entry.URL,
			Headers:  entry.Headers,
			OnNotice: func(line string) { log.Event(entry.Name, line) },
		})
	} else {
		tr, err = transport.NewStdio(transport.StdioConfig{
			Command:  entry.Command,
			Args:     entry.Args,
			Env:      entry.Env,
			Dir:      entry.Dir,
			OnStderr: func(line string) { log.Event(entry.Name, "stderr: "+line) },
			OnNotice: func(line string) { log.Event(entry.Name, line) },
		})
	}
	if err != nil {
		result.Err = err
		return result
	}

	session := mcp.NewSession(ctx, mcp.SessionConfig{
		Name:      entry.Name,
		Transport: tr,
		LogFrame:  log.For(entry.Name),
		OnNotification: func(method string, _ json.RawMessage) {
			log.Event(entry.Name, "notification: "+method)
		},
	})

	// The handshake gets a deadline of its own: npx and uvx may download a
	// package the first time a server runs, which the rest of the session never
	// has to wait for again.
	startCtx, cancel := context.WithTimeout(ctx, config.DefaultStartupTimeout)
	defer cancel()

	info, err := session.Initialize(startCtx)
	if err != nil {
		result.Err = err
		_ = session.Close()
		return result
	}

	result.Server = info.ServerInfo
	result.Version = session.ProtocolVersion()

	tools, err := reg.Add(startCtx, session)
	if err != nil {
		result.Err = err
		_ = session.Close()
		return result
	}
	result.Tools = tools

	return result
}

// describe renders one connection for the startup banner.
func describe(c connection, elapsed time.Duration) string {
	if !c.OK() {
		return fmt.Sprintf("  %-14s unavailable: %v", c.Name, c.Err)
	}
	return fmt.Sprintf("  %-14s %-5s %s %s  (protocol %s, %d tools, %s)",
		c.Name, c.Transport, c.Server.Name, c.Server.Version, c.Version, len(c.Tools),
		elapsed.Round(time.Millisecond))
}
