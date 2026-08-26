// Package jsonrpc implements the subset of JSON-RPC 2.0 that the Model Context
// Protocol relies on.
//
// The protocol is spoken directly over the wire format instead of through an
// MCP SDK, so every frame sent to or received from a server is built and parsed
// here and can be logged verbatim.
package jsonrpc

import (
	"encoding/json"
	"fmt"
)

// Version is the only JSON-RPC version MCP allows.
const Version = "2.0"

// Error codes defined by the JSON-RPC 2.0 specification.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Message is the single envelope used for every JSON-RPC frame.
//
// Requests, responses and notifications share one wire shape and are told apart
// by which fields are present, so a decoder cannot know in advance which kind of
// message has arrived. That is why this is one struct and not three: the
// classification happens after unmarshalling, through IsRequest, IsResponse and
// IsNotification.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// IsRequest reports whether the message is a request: it names a method and
// carries an id, so the sender expects a response.
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether the message is a notification: a method call
// without an id. A notification must never be answered, not even with an error.
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether the message answers a request: it carries an id
// but names no method.
func (m *Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// Error is a JSON-RPC error object. It reports a failure of the protocol
// itself: an unknown method, malformed parameters, or an internal fault.
//
// A tool that runs and fails is not this. That case comes back as a successful
// response whose result has isError set, because the failure is meant for the
// model to read and react to, not for the transport layer to raise.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("jsonrpc error %d: %s (%s)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// NewRequest builds a request carrying the given numeric id. params may be nil,
// in which case the field is omitted entirely rather than sent as null.
func NewRequest(id int64, method string, params any) (*Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{
		JSONRPC: Version,
		ID:      json.RawMessage(fmt.Sprintf("%d", id)),
		Method:  method,
		Params:  raw,
	}, nil
}

// NewNotification builds a notification: a method call with no id, which the
// peer must not answer.
func NewNotification(method string, params any) (*Message, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	return &Message{
		JSONRPC: Version,
		Method:  method,
		Params:  raw,
	}, nil
}

// NewErrorResponse builds a response rejecting a request the peer sent us. The
// id must be echoed back unchanged so the peer can match it to its own pending
// call.
func NewErrorResponse(id json.RawMessage, code int, message string) *Message {
	return &Message{
		JSONRPC: Version,
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return raw, nil
}
