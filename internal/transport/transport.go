// Package transport carries raw JSON-RPC frames between this host and an MCP
// server.
//
// MCP defines two transports. Local servers run as child processes and exchange
// newline-delimited JSON over stdin and stdout. Remote servers are reached over
// Streamable HTTP. Both reduce to the same contract, so the JSON-RPC client
// above them never learns which one it is talking to.
package transport

import (
	"context"
	"errors"
)

// ErrClosed is returned once a transport has been shut down.
var ErrClosed = errors.New("transport: closed")

// Transport moves complete JSON-RPC frames in both directions.
//
// Write may be called concurrently. Read is expected to be driven by a single
// goroutine, which is how the JSON-RPC client uses it.
type Transport interface {
	// Write sends one complete frame.
	Write(ctx context.Context, frame []byte) error

	// Read blocks until the next frame arrives. It returns an error once the
	// peer has gone away and no further frames can arrive.
	Read(ctx context.Context) ([]byte, error)

	// Close shuts the transport down and releases its resources. It is safe to
	// call more than once.
	Close() error
}
