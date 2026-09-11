package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// MCP's Streamable HTTP headers.
const (
	headerSessionID       = "Mcp-Session-Id"
	headerProtocolVersion = "MCP-Protocol-Version"
)

// Content types a response may use.
const (
	contentJSON = "application/json"
	contentSSE  = "text/event-stream"
)

// errorBodyLimit caps how much of a failed response is quoted back, so a server
// that answers an HTML error page does not fill the log with it.
const errorBodyLimit = 512

// HTTP speaks the MCP Streamable HTTP transport against a remote server.
//
// One endpoint carries everything. A frame is POSTed, and the reply is either a
// single JSON frame, a stream of frames as server-sent events, or nothing at
// all when the frame was a notification. All three end up in the same queue, so
// the JSON-RPC client above cannot tell this apart from a local process.
type HTTP struct {
	endpoint string
	client   *http.Client
	headers  map[string]string

	frames chan []byte
	done   chan struct{}

	onNotice func(line string)

	mu        sync.Mutex
	sessionID string
	version   string
	closed    bool

	ctx      context.Context
	cancel   context.CancelFunc
	requests sync.WaitGroup
}

// HTTPConfig configures an HTTP transport.
type HTTPConfig struct {
	// Endpoint is the server's MCP URL.
	Endpoint string

	// Client, if set, replaces the default. It must not impose a timeout:
	// a server-sent event stream stays open for as long as the server wants.
	Client *http.Client

	// Headers are sent on every request, for API keys and the like.
	Headers map[string]string

	// OnNotice receives transport-level warnings.
	OnNotice func(line string)
}

// NewHTTP returns a transport for a remote server. No request is made until the
// first frame is written.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("transport: no endpoint given")
	}

	client := cfg.Client
	if client == nil {
		// No Timeout on purpose: it would cut an event stream mid-session.
		// Deadlines come from the context of each call instead.
		client = &http.Client{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &HTTP{
		ctx:      ctx,
		cancel:   cancel,
		endpoint: cfg.Endpoint,
		client:   client,
		headers:  cfg.Headers,
		frames:   make(chan []byte, 16),
		done:     make(chan struct{}),
		onNotice: cfg.OnNotice,
	}, nil
}

// Endpoint returns the URL this transport talks to, for logging.
func (h *HTTP) Endpoint() string { return h.endpoint }

// SessionID returns the session the server assigned, if it assigned one.
func (h *HTTP) SessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionID
}

func (h *HTTP) Write(ctx context.Context, frame []byte) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	// Register before releasing the lock so Close cannot race Add against Wait.
	h.requests.Add(1)
	h.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	finish := func() { stop(); cancel(); h.requests.Done() }
	streaming := false
	defer func() {
		if !streaming {
			finish()
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("transport: build request: %w", err)
	}
	h.applyHeaders(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("transport: post frame: %w", err)
	}

	// A session id arrives with the initialize response and has to be echoed on
	// every request after it, or the server treats the next one as a stranger.
	if id := resp.Header.Get(headerSessionID); id != "" {
		h.setSessionID(id)
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode < 400 && resp.StatusCode != http.StatusAccepted && mediaType == contentSSE {
		streaming = true
		go func() { defer finish(); h.drainStream(resp) }()
		return nil
	}
	return h.handleResponse(resp)
}

func (h *HTTP) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-h.frames:
		if !ok {
			return nil, ErrClosed
		}
		return frame, nil
	}
}

// Close ends the session. The server is told explicitly, because a session it
// still believes is open holds resources on the far end.
func (h *HTTP) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	sessionID := h.sessionID
	h.mu.Unlock()

	close(h.done)
	h.cancel() // Interrupt idle response-body reads as well as pending POSTs.
	h.requests.Wait()
	close(h.frames)

	if sessionID == "" {
		return nil
	}
	return h.deleteSession(sessionID)
}

// handleResponse turns whatever came back into frames.
func (h *HTTP) handleResponse(resp *http.Response) error {
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return h.statusError(resp)
	}

	// 202 answers a notification or a response: there is nothing to return, and
	// the specification says the body is empty.
	if resp.StatusCode == http.StatusAccepted {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	defer resp.Body.Close()
	return h.readSingleFrame(resp.Body)
}

func (h *HTTP) readSingleFrame(body io.Reader) error {
	frame, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("transport: read response: %w", err)
	}
	if frame = bytes.TrimSpace(frame); len(frame) == 0 {
		return nil
	}

	h.captureVersion(frame)
	h.push(frame)
	return nil
}

// drainStream consumes an event stream until the server closes it.
func (h *HTTP) drainStream(resp *http.Response) {
	defer resp.Body.Close()

	reader := newSSEReader(resp.Body)
	for {
		payload, err := reader.next()
		if err != nil {
			if err != io.EOF {
				h.notice("event stream ended: " + err.Error())
			}
			return
		}

		if frame := bytes.TrimSpace(payload); len(frame) > 0 {
			h.captureVersion(frame)
			if !h.push(frame) {
				return
			}
		}
	}
}

// push queues a frame, giving up if the transport is closing.
func (h *HTTP) push(frame []byte) bool {
	select {
	case h.frames <- frame:
		return true
	case <-h.done:
		return false
	}
}

func (h *HTTP) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", contentJSON)

	// Either shape is acceptable; the server picks one per response.
	req.Header.Set("Accept", contentJSON+", "+contentSSE)

	for key, value := range h.headers {
		req.Header.Set(key, value)
	}

	h.mu.Lock()
	sessionID, version := h.sessionID, h.version
	h.mu.Unlock()

	if sessionID != "" {
		req.Header.Set(headerSessionID, sessionID)
	}
	// The version header belongs on every request after initialization, so it
	// appears only once the server has answered with the version it agreed to.
	if version != "" {
		req.Header.Set(headerProtocolVersion, version)
	}
}

// captureVersion notes the protocol revision the server settled on.
//
// This is the one place the transport reads what it carries. The Streamable
// HTTP transport is defined in terms of the handshake — the session id and the
// version header both come out of it — so the coupling is the specification's,
// not this design's.
func (h *HTTP) captureVersion(frame []byte) {
	h.mu.Lock()
	already := h.version != ""
	h.mu.Unlock()
	if already {
		return
	}

	var envelope struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return
	}
	if envelope.Result.ProtocolVersion == "" {
		return
	}

	h.mu.Lock()
	h.version = envelope.Result.ProtocolVersion
	h.mu.Unlock()
}

func (h *HTTP) setSessionID(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessionID == "" {
		h.sessionID = id
	}
}

func (h *HTTP) deleteSession(sessionID string) error {
	// Session deletion is best-effort cleanup, not an unbounded model/tool call.
	// Reuse the local child shutdown grace as the cleanup budget.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.endpoint, nil)
	if err != nil {
		return nil
	}
	h.applyHeaders(req)
	req.Header.Set(headerSessionID, sessionID)

	resp, err := h.client.Do(req)
	if err != nil {
		// The session is over on this side regardless; a server that cannot be
		// reached to be told will time it out on its own.
		return nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (h *HTTP) statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fmt.Errorf("transport: %s returned %s", h.endpoint, resp.Status)
	}
	return fmt.Errorf("transport: %s returned %s: %s", h.endpoint, resp.Status, detail)
}

func (h *HTTP) notice(line string) {
	if h.onNotice != nil {
		h.onNotice(line)
	}
}

// compile-time proof that both transports satisfy the same contract.
var (
	_ Transport = (*HTTP)(nil)
	_ Transport = (*Stdio)(nil)
)
