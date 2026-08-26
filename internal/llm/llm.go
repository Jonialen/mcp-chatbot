// Package llm defines the boundary between the chatbot and whatever model
// answers it.
//
// Nothing below this package knows which provider is in use, and nothing in
// this package knows about MCP. That separation is the reason the protocol
// layer survives a change of model vendor untouched: a provider is swapped by
// implementing Provider, not by editing the transport, the JSON-RPC client or
// the MCP session.
package llm

import (
	"context"
	"encoding/json"
)

// Role identifies who produced a message.
type Role string

const (
	// RoleUser is the person talking to the chatbot.
	RoleUser Role = "user"
	// RoleModel is the model's own turn.
	RoleModel Role = "model"
)

// ToolDef describes one callable offered to the model.
//
// InputSchema is kept as raw JSON Schema, exactly as the MCP server published
// it. Providers differ in what they accept, so narrowing the schema is each
// adapter's problem and never the caller's.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolCall is the model asking for a tool to run.
//
// ID is what the provider uses to pair a call with its result. Not every
// provider issues one; when it is empty the pairing falls back to Name, which
// is what the providers that omit an id expect.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any

	// ProviderState is opaque data the provider attached to this call and
	// expects back verbatim when the conversation continues.
	//
	// Reasoning models sign their tool calls and reject a replayed history in
	// which the signature is missing, so a call cannot be reconstructed from
	// name and arguments alone. The host never inspects this; it only has to
	// carry it, which is why it is bytes and not a provider type. Dropping it
	// is what makes the second turn of an agent loop fail.
	ProviderState []byte
}

// ToolResult is what a tool produced, on its way back to the model.
//
// IsError carries a tool that ran and failed. It is deliberately not an error
// value: the model is supposed to read the failure and try something else, so
// it has to travel as part of the conversation.
type ToolResult struct {
	ID      string
	Name    string
	Content string
	IsError bool
}

// Message is one turn of the conversation.
//
// A turn holds either text, tool calls, or tool results. The model's turn is
// what carries ToolCalls; the reply to it is a turn carrying ToolResults.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Request is one call to the model.
type Request struct {
	// System is the instruction that frames the whole conversation.
	System string

	// Messages is the conversation so far, oldest first. Keeping the history
	// in the request rather than inside the provider is what lets a session be
	// replayed, logged or trimmed without the provider's cooperation.
	Messages []Message

	// Tools is everything the model may call this turn.
	Tools []ToolDef

	// MaxOutputTokens caps the reply. Zero means the provider's default.
	MaxOutputTokens int
}

// Usage reports what a call cost, so a session can be budgeted.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is one reply from the model.
//
// Text and ToolCalls are not exclusive: a model may explain itself and request
// a tool in the same turn.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// WantsTools reports whether the model asked for a tool, which is the signal to
// run another round of the agent loop instead of returning to the user.
func (r *Response) WantsTools() bool { return len(r.ToolCalls) > 0 }

// Provider is a model that can hold a conversation and request tools.
type Provider interface {
	// Name identifies the vendor, for logs and for the report.
	Name() string

	// Model identifies the specific model in use.
	Model() string

	// Generate sends one request and returns the model's reply.
	Generate(ctx context.Context, req Request) (*Response, error)
}
