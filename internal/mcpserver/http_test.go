package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
	"github.com/Jonialen/mcp-chatbot/internal/mcp"
)

func post(t *testing.T, handler http.Handler, frame string, headers map[string]string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(frame))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder.Result()
}

func TestHTTPHandlerMintsSessionOnInitialize(t *testing.T) {
	handler := NewHTTPHandler(testServer(t))

	resp := post(t, handler,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}
	if resp.Header.Get("Mcp-Session-Id") == "" {
		t.Error("initialize did not mint a session id")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}

	var msg jsonrpc.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !msg.IsResponse() {
		t.Errorf("the body is not a JSON-RPC response: %+v", msg)
	}
}

// A notification has no reply, and 202 says the empty body is deliberate.
func TestHTTPHandlerAnswers202ToNotification(t *testing.T) {
	handler := NewHTTPHandler(testServer(t))

	resp := post(t, handler, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %s, want 202", resp.Status)
	}
}

func TestHTTPHandlerCallsTools(t *testing.T) {
	handler := NewHTTPHandler(testServer(t))

	resp := post(t, handler,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"over http"}}}`,
		nil)
	defer resp.Body.Close()

	var msg jsonrpc.Message
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var result mcp.CallToolResult
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Text() != "over http" {
		t.Errorf("Text() = %q", result.Text())
	}
}

func TestHTTPHandlerRejectsMalformedFrames(t *testing.T) {
	handler := NewHTTPHandler(testServer(t))

	resp := post(t, handler, `{not json`, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}

// A client may end its session explicitly, and the server must accept that
// rather than hold the state until it times out.
func TestHTTPHandlerAcceptsSessionDelete(t *testing.T) {
	handler := NewHTTPHandler(testServer(t))

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "whatever")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", recorder.Code)
	}
}

// This server pushes nothing, so it declines the stream rather than holding a
// connection open forever for no messages.
func TestHTTPHandlerDeclinesServerPushStream(t *testing.T) {
	handler := NewHTTPHandler(testServer(t))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", recorder.Code)
	}
}

func TestSessionStoreCreatesUniqueIDsAndDrops(t *testing.T) {
	store := newSessionStore()

	seen := make(map[string]bool)
	for range 100 {
		id := store.create()
		if id == "" {
			t.Fatal("create returned an empty id")
		}
		if seen[id] {
			t.Fatalf("create returned the duplicate id %q", id)
		}
		seen[id] = true
	}

	var one string
	for id := range seen {
		one = id
		break
	}
	store.drop(one)

	store.mu.Lock()
	_, present := store.lastSeen[one]
	store.mu.Unlock()
	if present {
		t.Error("drop did not remove the session")
	}

	// Dropping nothing must not panic.
	store.drop("")
}

// A client that hangs up must cancel the work its request started, or a slow
// tool keeps running for a caller that is already gone.
//
// This runs against a real server: httptest.NewRequest builds a request whose
// context is never cancelled, so it cannot show the behaviour being checked.
func TestClientDisconnectCancelsTheTool(t *testing.T) {
	server := New("ctx", "1.0.0")

	running := make(chan struct{})
	cancelled := make(chan error, 1)

	server.Register(Tool{
		Name:        "slow",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(running)
			<-ctx.Done()
			cancelled <- ctx.Err()
			return "", ctx.Err()
		},
	})

	httpServer := httptest.NewServer(NewHTTPHandler(server))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the tool never started")
	}

	cancel()

	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the tool saw %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("hanging up did not cancel the tool")
	}
}
