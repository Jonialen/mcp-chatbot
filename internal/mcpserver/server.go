// Package mcpserver is the server half of the Model Context Protocol.
//
// It is written against the JSON-RPC wire format directly, like the client
// half, so both sides of every frame in this project belong to it. A server
// registers tools and is then served over either transport: stdio for a local
// server launched as a child process, HTTP for a remote one.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

// Handler runs one tool. Arguments arrive as raw JSON so a handler can decode
// them into whatever shape its schema promised.
//
// Returning an error means the tool failed, and the failure is reported to the
// model as a tool result with isError set rather than as a protocol error. A
// handler should return an error for a bad path or a missing record, and should
// not treat that as exceptional.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool is one callable this server publishes.
type Tool struct {
	Name        string
	Title       string
	Description string

	// InputSchema is a JSON Schema object describing the arguments. It is
	// published verbatim, and it is what the model reads to build a call.
	InputSchema json.RawMessage

	Handler Handler
}

// Server publishes a set of tools over MCP.
type Server struct {
	info mcp.Implementation

	mu    sync.RWMutex
	tools map[string]Tool
	order []string
}

// New returns a Server identified by the given name and version.
func New(name, version string) *Server {
	return &Server{
		info:  mcp.Implementation{Name: name, Version: version},
		tools: make(map[string]Tool),
	}
}

// Register adds a tool. Registering the same name twice is a programming error
// and panics, because it means two tools would silently shadow each other.
func (s *Server) Register(tool Tool) {
	if tool.Name == "" {
		panic("mcpserver: a tool must have a name")
	}
	if tool.Handler == nil {
		panic("mcpserver: tool " + tool.Name + " has no handler")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, taken := s.tools[tool.Name]; taken {
		panic("mcpserver: tool " + tool.Name + " is already registered")
	}
	s.tools[tool.Name] = tool
	s.order = append(s.order, tool.Name)
	sort.Strings(s.order)
}

// Handle answers one incoming message.
//
// It returns nil when there is nothing to send back, which is the case for a
// notification: answering one is a protocol violation, not a courtesy.
func (s *Server) Handle(ctx context.Context, msg *jsonrpc.Message) *jsonrpc.Message {
	if msg.IsNotification() {
		return nil
	}
	if !msg.IsRequest() {
		// A response arriving at a server that issued no request.
		return nil
	}

	switch msg.Method {
	case mcp.MethodInitialize:
		return s.initialize(msg)
	case mcp.MethodListTools:
		return s.listTools(msg)
	case mcp.MethodCallTool:
		return s.callTool(ctx, msg)
	default:
		return jsonrpc.NewErrorResponse(msg.ID, jsonrpc.CodeMethodNotFound,
			"unknown method: "+msg.Method)
	}
}

func (s *Server) initialize(msg *jsonrpc.Message) *jsonrpc.Message {
	// The client's requested version is echoed back when this server can speak
	// it, so a client built against an earlier revision is not turned away over
	// a tools surface that has not changed.
	version := mcp.ProtocolVersion
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(msg.Params, &params); err == nil && params.ProtocolVersion != "" {
		version = params.ProtocolVersion
	}

	return result(msg.ID, map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      s.info,
	})
}

func (s *Server) listTools(msg *jsonrpc.Message) *jsonrpc.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tools := make([]mcp.Tool, 0, len(s.order))
	for _, name := range s.order {
		tool := s.tools[name]
		tools = append(tools, mcp.Tool{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
		})
	}
	return result(msg.ID, map[string]any{"tools": tools})
}

func (s *Server) callTool(ctx context.Context, msg *jsonrpc.Message) *jsonrpc.Message {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return jsonrpc.NewErrorResponse(msg.ID, jsonrpc.CodeInvalidParams,
			"malformed parameters: "+err.Error())
	}

	s.mu.RLock()
	tool, known := s.tools[params.Name]
	s.mu.RUnlock()

	if !known {
		// An unknown tool is a protocol error: the client asked for something
		// this server never published.
		return jsonrpc.NewErrorResponse(msg.ID, jsonrpc.CodeInvalidParams,
			"unknown tool: "+params.Name)
	}

	output, err := tool.Handler(ctx, params.Arguments)
	if err != nil {
		// A tool that ran and failed is reported inside a successful response.
		// The failure is for the model to read and work around, so it travels
		// as content rather than as a fault of the protocol.
		return result(msg.ID, toolResult(err.Error(), true))
	}
	return result(msg.ID, toolResult(output, false))
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func result(id json.RawMessage, payload any) *jsonrpc.Message {
	raw, err := json.Marshal(payload)
	if err != nil {
		return jsonrpc.NewErrorResponse(id, jsonrpc.CodeInternalError,
			fmt.Sprintf("encode result: %v", err))
	}
	return &jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: id, Result: raw}
}
