package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
	"github.com/Jonialen/mcp-chatbot/internal/transport"
)

// fakeServer is a scripted MCP server: it records every frame the client sends
// and answers according to respond.
type fakeServer struct {
	respond func(msg jsonrpc.Message) string

	replies chan []byte
	closed  chan struct{}
	once    sync.Once

	mu   sync.Mutex
	seen [][]byte
}

func newFakeServer(respond func(msg jsonrpc.Message) string) *fakeServer {
	return &fakeServer{
		respond: respond,
		replies: make(chan []byte, 32),
		closed:  make(chan struct{}),
	}
}

func (f *fakeServer) Write(ctx context.Context, frame []byte) error {
	dup := make([]byte, len(frame))
	copy(dup, frame)

	f.mu.Lock()
	f.seen = append(f.seen, dup)
	f.mu.Unlock()

	var msg jsonrpc.Message
	if err := json.Unmarshal(dup, &msg); err != nil {
		return err
	}
	if reply := f.respond(msg); reply != "" {
		select {
		case f.replies <- []byte(reply):
		case <-f.closed:
		}
	}
	return nil
}

func (f *fakeServer) Read(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-f.replies:
		return frame, nil
	case <-f.closed:
		return nil, transport.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeServer) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// frames returns every frame the client sent, as raw JSON.
func (f *fakeServer) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.seen))
	copy(out, f.seen)
	return out
}

func newTestSession(t *testing.T, respond func(msg jsonrpc.Message) string) (*Session, *fakeServer) {
	t.Helper()

	server := newFakeServer(respond)
	ctx, cancel := context.WithCancel(context.Background())

	session := NewSession(ctx, SessionConfig{Name: "fake", Transport: server})
	t.Cleanup(func() {
		cancel()
		_ = session.Close()
	})
	return session, server
}

func okResult(id json.RawMessage, result string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
}

const emptyInitResult = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},` +
	`"serverInfo":{"name":"fake-server","version":"1.0.0"}}`

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The handshake is three messages, not two. A server may reject work until the
// client confirms it is ready, so the trailing notification is not optional.
func TestInitializeCompletesThreeMessageHandshake(t *testing.T) {
	session, server := newTestSession(t, func(msg jsonrpc.Message) string {
		if msg.Method == MethodInitialize {
			return okResult(msg.ID, emptyInitResult)
		}
		return ""
	})

	result, err := session.Initialize(testCtx(t))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !result.SupportsTools() {
		t.Error("SupportsTools() = false, want true")
	}
	if session.ServerInfo().Name != "fake-server" {
		t.Errorf("ServerInfo().Name = %q", session.ServerInfo().Name)
	}

	frames := server.frames()
	if len(frames) != 2 {
		t.Fatalf("client sent %d frames, want 2 (initialize, initialized)", len(frames))
	}

	var confirm jsonrpc.Message
	if err := json.Unmarshal(frames[1], &confirm); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if confirm.Method != MethodInitialized {
		t.Errorf("second frame method = %q, want %q", confirm.Method, MethodInitialized)
	}
	if !confirm.IsNotification() {
		t.Errorf("initialized must be a notification, got %s", frames[1])
	}
}

// A classmate's server built against an earlier revision still speaks a tools
// surface this client understands. Refusing it on a version mismatch would cost
// interoperability for nothing.
func TestInitializeAcceptsOlderProtocolVersion(t *testing.T) {
	session, _ := newTestSession(t, func(msg jsonrpc.Message) string {
		if msg.Method == MethodInitialize {
			return okResult(msg.ID, `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},`+
				`"serverInfo":{"name":"old-server","version":"0.1.0"}}`)
		}
		return ""
	})

	if _, err := session.Initialize(testCtx(t)); err != nil {
		t.Fatalf("Initialize with an older protocol version: %v", err)
	}
	if got := session.ProtocolVersion(); got != "2024-11-05" {
		t.Errorf("ProtocolVersion() = %q, want the version the server answered with", got)
	}
}

// Regression: the arguments field was omitted for a tool that takes none, and
// the official filesystem server rejected the call with -32602 because its
// schema requires an object.
func TestCallToolAlwaysSendsArgumentsObject(t *testing.T) {
	session, server := newTestSession(t, func(msg jsonrpc.Message) string {
		switch msg.Method {
		case MethodInitialize:
			return okResult(msg.ID, emptyInitResult)
		case MethodCallTool:
			return okResult(msg.ID, `{"content":[{"type":"text","text":"ok"}]}`)
		}
		return ""
	})

	ctx := testCtx(t)
	if _, err := session.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := session.CallTool(ctx, "no_arguments_tool", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	var call []byte
	for _, frame := range server.frames() {
		if strings.Contains(string(frame), MethodCallTool) {
			call = frame
		}
	}
	if call == nil {
		t.Fatal("no tools/call frame was sent")
	}

	var msg jsonrpc.Message
	if err := json.Unmarshal(call, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	args, present := params["arguments"]
	if !present {
		t.Fatalf("tools/call omitted the arguments field: %s", call)
	}
	if string(args) != "{}" {
		t.Errorf("arguments = %s, want an empty object", args)
	}
}

// A tool that ran and failed is not a protocol error. It arrives as a
// successful result with isError set, so the model can read it and recover.
func TestCallToolSurfacesToolFailureAsResult(t *testing.T) {
	session, _ := newTestSession(t, func(msg jsonrpc.Message) string {
		switch msg.Method {
		case MethodInitialize:
			return okResult(msg.ID, emptyInitResult)
		case MethodCallTool:
			return okResult(msg.ID,
				`{"content":[{"type":"text","text":"file not found"}],"isError":true}`)
		}
		return ""
	})

	ctx := testCtx(t)
	if _, err := session.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	result, err := session.CallTool(ctx, "read_file", map[string]any{"path": "/nope"})
	if err != nil {
		t.Fatalf("CallTool returned an error for a failed tool, want a result: %v", err)
	}
	if !result.IsError {
		t.Error("IsError = false, want true")
	}
	if result.Text() != "file not found" {
		t.Errorf("Text() = %q", result.Text())
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	session, _ := newTestSession(t, func(msg jsonrpc.Message) string {
		switch msg.Method {
		case MethodInitialize:
			return okResult(msg.ID, emptyInitResult)
		case MethodListTools:
			var params ListToolsParams
			_ = json.Unmarshal(msg.Params, &params)
			if params.Cursor == "" {
				return okResult(msg.ID,
					`{"tools":[{"name":"first","inputSchema":{}}],"nextCursor":"page2"}`)
			}
			return okResult(msg.ID, `{"tools":[{"name":"second","inputSchema":{}}]}`)
		}
		return ""
	})

	ctx := testCtx(t)
	if _, err := session.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	tools, err := session.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want both pages", len(tools))
	}
	if tools[0].Name != "first" || tools[1].Name != "second" {
		t.Errorf("tools = %v", tools)
	}
}

// A server that keeps handing back the same cursor would otherwise spin this
// client forever.
func TestListToolsRejectsRepeatedCursor(t *testing.T) {
	session, _ := newTestSession(t, func(msg jsonrpc.Message) string {
		switch msg.Method {
		case MethodInitialize:
			return okResult(msg.ID, emptyInitResult)
		case MethodListTools:
			return okResult(msg.ID, `{"tools":[],"nextCursor":"stuck"}`)
		}
		return ""
	})

	ctx := testCtx(t)
	if _, err := session.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := session.ListTools(ctx); err == nil {
		t.Fatal("ListTools looped on a repeated cursor instead of failing")
	}
}
