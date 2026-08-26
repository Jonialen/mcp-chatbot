package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Jonialen/mcp-chatbot/internal/transport"
)

// Direction says which way a frame travelled, for logging.
type Direction string

const (
	Outbound Direction = "out"
	Inbound  Direction = "in"
)

// FrameLogger receives every frame that crosses the transport, exactly as it
// appeared on the wire.
type FrameLogger func(dir Direction, frame []byte)

// NotificationHandler receives notifications sent by the server. Notifications
// carry no id and must never be answered.
type NotificationHandler func(method string, params json.RawMessage)

// Client speaks JSON-RPC 2.0 over a transport.
//
// A single goroutine, started by Run, owns reading. It matches each incoming
// response to the call waiting for it through the pending map, which is the
// whole of the request/response correlation: an id is minted per call, a
// one-slot channel is parked under that id, and the reader hands the response
// over and moves on.
type Client struct {
	transport transport.Transport
	logFrame  FrameLogger
	onNotify  NotificationHandler

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[string]chan *Message

	closeOnce sync.Once
	closed    chan struct{}
	runErr    error
}

// ClientConfig configures a Client.
type ClientConfig struct {
	Transport transport.Transport

	// LogFrame, if set, is called for every frame in both directions.
	LogFrame FrameLogger

	// OnNotification, if set, receives server-initiated notifications.
	OnNotification NotificationHandler
}

// NewClient wraps a transport. Nothing is read until Run is called.
func NewClient(cfg ClientConfig) *Client {
	return &Client{
		transport: cfg.Transport,
		logFrame:  cfg.LogFrame,
		onNotify:  cfg.OnNotification,
		pending:   make(map[string]chan *Message),
		closed:    make(chan struct{}),
	}
}

// Run reads frames until the transport fails or ctx is cancelled. It is meant
// to be run in its own goroutine and returns the error that ended the session.
func (c *Client) Run(ctx context.Context) error {
	for {
		frame, err := c.transport.Read(ctx)
		if err != nil {
			c.shutdown(err)
			return err
		}

		c.log(Inbound, frame)

		var msg Message
		if err := json.Unmarshal(frame, &msg); err != nil {
			// A frame that will not parse is dropped rather than fatal. The
			// session stays usable and the raw bytes are already in the log.
			continue
		}
		c.dispatch(ctx, &msg)
	}
}

// Call sends a request and blocks until the matching response arrives, ctx is
// cancelled, or the session ends.
//
// A JSON-RPC error in the response is returned as an *Error. A tool that ran
// and failed is not an error here: that arrives as a normal result whose
// isError flag is set, and the caller decides what to do with it.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req, err := NewRequest(c.nextID.Add(1), method, params)
	if err != nil {
		return nil, err
	}

	key := idKey(req.ID)
	// The channel is buffered so the reader can deliver a late response and
	// move on even if this call has already given up.
	inbox := make(chan *Message, 1)

	c.mu.Lock()
	c.pending[key] = inbox
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	if err := c.send(ctx, req); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	case <-c.closed:
		return nil, fmt.Errorf("%s: %w", method, c.sessionErr())
	case resp := <-inbox:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// Notify sends a notification. There is no reply to wait for.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	msg, err := NewNotification(method, params)
	if err != nil {
		return err
	}
	return c.send(ctx, msg)
}

// Close ends the session and releases every waiting call.
func (c *Client) Close() error {
	c.shutdown(transport.ErrClosed)
	return c.transport.Close()
}

func (c *Client) dispatch(ctx context.Context, msg *Message) {
	switch {
	case msg.IsResponse():
		c.deliver(msg)

	case msg.IsNotification():
		if c.onNotify != nil {
			c.onNotify(msg.Method, msg.Params)
		}

	case msg.IsRequest():
		// The server is asking us for something: sampling, roots, elicitation.
		// This host implements none of them, so it answers with the error the
		// specification defines for an unknown method.
		//
		// Staying silent would be worse than refusing. The server would keep
		// waiting for a response that is never coming, and whatever it was
		// doing on our behalf would stall.
		resp := NewErrorResponse(msg.ID, CodeMethodNotFound,
			"method not implemented by this client: "+msg.Method)
		_ = c.send(ctx, resp)
	}
}

func (c *Client) deliver(msg *Message) {
	key := idKey(msg.ID)

	c.mu.Lock()
	inbox, waiting := c.pending[key]
	c.mu.Unlock()

	if !waiting {
		// A response nobody is waiting for: the call timed out and gave up
		// before the server answered. Dropping it is correct.
		return
	}
	inbox <- msg
}

func (c *Client) send(ctx context.Context, msg *Message) error {
	frame, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	c.log(Outbound, frame)
	return c.transport.Write(ctx, frame)
}

func (c *Client) log(dir Direction, frame []byte) {
	if c.logFrame == nil {
		return
	}
	// The logger gets its own copy: the transport may reuse the buffer, and a
	// log that mutates after the fact is worse than no log.
	dup := make([]byte, len(frame))
	copy(dup, frame)
	c.logFrame(dir, dup)
}

func (c *Client) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.runErr = err
		close(c.closed)
	})
}

func (c *Client) sessionErr() error {
	if c.runErr != nil {
		return c.runErr
	}
	return transport.ErrClosed
}

// idKey normalises an id into a map key.
//
// The specification requires a server to echo the id unchanged, and this client
// only ever sends integers. A few servers still round-trip the value through a
// float or a string, so 1, 1.0 and "1" are folded onto the same key rather than
// leaving a call waiting forever for a response that already arrived.
func idKey(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return s
}

// IsMethodNotFound reports whether err is the JSON-RPC error a server returns
// for a capability it does not implement.
func IsMethodNotFound(err error) bool {
	var rpcErr *Error
	return errors.As(err, &rpcErr) && rpcErr.Code == CodeMethodNotFound
}
