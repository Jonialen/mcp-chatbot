// Package mcp implements the Model Context Protocol on top of a raw JSON-RPC
// client.
//
// Only the tools surface is implemented, which is what this host needs: the
// initialization handshake, tools/list and tools/call. Resources, prompts,
// sampling and elicitation are deliberately left out rather than stubbed.
package mcp

import "encoding/json"

// ProtocolVersion is the revision this client asks for during the handshake.
// A server is free to answer with a different one; see Session.Initialize.
const ProtocolVersion = "2025-06-18"

// Method names used by this client.
const (
	MethodInitialize  = "initialize"
	MethodInitialized = "notifications/initialized"
	MethodListTools   = "tools/list"
	MethodCallTool    = "tools/call"
)

// Implementation identifies one side of the connection.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities announces what this host can do for a server.
//
// It is empty on purpose. This host implements no roots, sampling or
// elicitation, and advertising a capability it cannot serve would invite
// requests it would then have to refuse.
type ClientCapabilities struct{}

// InitializeParams opens the handshake.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the server's half of the handshake.
//
// Capabilities is left as raw JSON: this client only needs to know whether the
// tools key is present, and decoding the rest into a struct would mean tracking
// every capability the specification grows.
type InitializeResult struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	ServerInfo      Implementation             `json:"serverInfo"`
	Instructions    string                     `json:"instructions,omitempty"`
}

// SupportsTools reports whether the server advertised the tools capability.
func (r *InitializeResult) SupportsTools() bool {
	_, ok := r.Capabilities["tools"]
	return ok
}

// Tool is one callable exposed by a server. InputSchema is a JSON Schema object
// kept verbatim, because it is handed straight to the model as the tool's
// parameter definition.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListToolsParams asks for a page of tools.
type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is one page of tools. A non-empty NextCursor means there is
// another page.
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams invokes a tool by name.
//
// Arguments is always sent, even when a tool takes none. The specification
// allows omitting it, but servers that validate their input against a JSON
// Schema of type object reject a missing field outright, and an empty object
// costs nothing for the servers that do not care.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Content is one block of a tool's output. Only text is decoded here; other
// block types keep their raw form so nothing is lost in the log.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallToolResult is what a tool returned.
//
// IsError deserves attention. A tool that ran and failed does not come back as
// a JSON-RPC error: it comes back here, as a perfectly successful response with
// this flag set. That is deliberate. The failure is meant for the model to read
// and recover from, so it has to travel as content, not as a transport fault.
type CallToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Text flattens the result's text blocks into a single string, which is the
// form a model expects as a tool result.
func (r *CallToolResult) Text() string {
	if len(r.Content) == 1 {
		return r.Content[0].Text
	}
	var out string
	for i, block := range r.Content {
		if block.Text == "" {
			continue
		}
		if i > 0 && out != "" {
			out += "\n"
		}
		out += block.Text
	}
	return out
}
