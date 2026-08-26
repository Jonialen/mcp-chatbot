package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
	"github.com/Jonialen/mcp-chatbot/internal/transport"
)

// hostInfo identifies this host to every server it connects to.
var hostInfo = Implementation{Name: "mcp-chatbot", Version: "0.1.0"}

// maxToolPages bounds pagination so a server that keeps handing back the same
// cursor cannot spin this client forever.
const maxToolPages = 100

// Session is one connection to one MCP server: the transport, the JSON-RPC
// client on top of it, and the protocol lifecycle around both.
type Session struct {
	name      string
	transport transport.Transport
	client    *jsonrpc.Client

	serverInfo      Implementation
	protocolVersion string
	instructions    string

	runErr chan error
}

// SessionConfig configures a Session.
type SessionConfig struct {
	// Name labels this server in logs and in tool names. It is the caller's
	// key for the server, not anything the server chose.
	Name string

	Transport transport.Transport

	// LogFrame, if set, receives every JSON-RPC frame in both directions.
	LogFrame jsonrpc.FrameLogger

	// OnNotification, if set, receives notifications sent by the server.
	OnNotification jsonrpc.NotificationHandler
}

// NewSession wraps a transport and starts the read loop. The handshake has not
// happened yet; call Initialize next.
func NewSession(ctx context.Context, cfg SessionConfig) *Session {
	client := jsonrpc.NewClient(jsonrpc.ClientConfig{
		Transport:      cfg.Transport,
		LogFrame:       cfg.LogFrame,
		OnNotification: cfg.OnNotification,
	})

	s := &Session{
		name:      cfg.Name,
		transport: cfg.Transport,
		client:    client,
		runErr:    make(chan error, 1),
	}

	go func() { s.runErr <- client.Run(ctx) }()

	return s
}

// Name returns the caller's label for this server.
func (s *Session) Name() string { return s.name }

// ServerInfo returns what the server said about itself during the handshake.
func (s *Session) ServerInfo() Implementation { return s.serverInfo }

// ProtocolVersion returns the revision the server answered with, which is not
// necessarily the one this client asked for.
func (s *Session) ProtocolVersion() string { return s.protocolVersion }

// Instructions returns the usage hint a server may send during the handshake.
func (s *Session) Instructions() string { return s.instructions }

// Initialize performs the MCP handshake.
//
// The exchange is three messages, not two: the client's initialize request, the
// server's result, and then a notification confirming the client is ready. A
// server is entitled to reject requests until that third message arrives, so
// skipping it produces a session that looks connected and answers nothing.
func (s *Session) Initialize(ctx context.Context) (*InitializeResult, error) {
	raw, err := s.client.Call(ctx, MethodInitialize, InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    ClientCapabilities{},
		ClientInfo:      hostInfo,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: initialize: %w", s.name, err)
	}

	var result InitializeResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: decode initialize result: %w", s.name, err)
	}

	// Whatever revision the server answers with is accepted. Servers written
	// against an earlier revision of the specification still speak a tools
	// surface this client understands, and refusing them on a version mismatch
	// would cost interoperability for no gain.
	s.protocolVersion = result.ProtocolVersion
	s.serverInfo = result.ServerInfo
	s.instructions = result.Instructions

	if err := s.client.Notify(ctx, MethodInitialized, nil); err != nil {
		return nil, fmt.Errorf("%s: confirm initialization: %w", s.name, err)
	}
	return &result, nil
}

// ListTools returns every tool the server exposes, following pagination until
// the server stops handing back a cursor.
func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	var (
		tools  []Tool
		cursor string
	)

	for page := 0; page < maxToolPages; page++ {
		raw, err := s.client.Call(ctx, MethodListTools, ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("%s: list tools: %w", s.name, err)
		}

		var result ListToolsResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("%s: decode tool list: %w", s.name, err)
		}
		tools = append(tools, result.Tools...)

		if result.NextCursor == "" {
			return tools, nil
		}
		if result.NextCursor == cursor {
			return nil, fmt.Errorf("%s: server repeated tool cursor %q", s.name, cursor)
		}
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("%s: tool list exceeded %d pages", s.name, maxToolPages)
}

// CallTool invokes a tool and returns its result.
//
// A tool that ran and failed is not an error here. It comes back as a result
// with IsError set, and the caller passes it to the model so the model can
// react to it. Only a protocol-level failure is returned as an error.
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}

	raw, err := s.client.Call(ctx, MethodCallTool, CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: call tool %q: %w", s.name, name, err)
	}

	var result CallToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("%s: decode result of tool %q: %w", s.name, name, err)
	}
	return &result, nil
}

// Close ends the session and shuts the server down.
func (s *Session) Close() error {
	return s.client.Close()
}
