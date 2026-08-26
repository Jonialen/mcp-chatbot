package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingServer is a stand-in MCP server reachable over HTTP.
type recordingServer struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string

	handler http.HandlerFunc
}

func (s *recordingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	s.mu.Lock()
	s.requests = append(s.requests, r.Clone(context.Background()))
	s.bodies = append(s.bodies, string(body))
	s.mu.Unlock()

	s.handler(w, r)
}

func (s *recordingServer) seen() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*http.Request, len(s.requests))
	copy(out, s.requests)
	return out
}

func newHTTPTransport(t *testing.T, handler http.HandlerFunc) (*HTTP, *recordingServer) {
	t.Helper()

	recorder := &recordingServer{handler: handler}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	tr, err := NewHTTP(HTTPConfig{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	return tr, recorder
}

func TestHTTPReadsSingleJSONResponse(t *testing.T) {
	tr, _ := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentJSON)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
	})

	ctx := testContext(t)
	if err := tr.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	frame, err := tr.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(frame) != `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` {
		t.Errorf("frame = %s", frame)
	}
}

// The same POST may answer with a stream instead, and every frame in it has to
// reach the queue.
func TestHTTPReadsFramesFromEventStream(t *testing.T) {
	tr, _ := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentSSE)
		fmt.Fprint(w, "event: message\n")
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":1,"result":{"step":1}}`+"\n\n")
		fmt.Fprint(w, "event: message\n")
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":2,"result":{"step":2}}`+"\n\n")
	})

	ctx := testContext(t)
	if err := tr.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for want := 1; want <= 2; want++ {
		frame, err := tr.Read(ctx)
		if err != nil {
			t.Fatalf("Read %d: %v", want, err)
		}
		var msg struct {
			Result struct {
				Step int `json:"step"`
			} `json:"result"`
		}
		if err := json.Unmarshal(frame, &msg); err != nil {
			t.Fatalf("unmarshal %s: %v", frame, err)
		}
		if msg.Result.Step != want {
			t.Errorf("got step %d, want %d", msg.Result.Step, want)
		}
	}
}

// A notification has no reply, and the server says so with 202.
func TestHTTPAcceptsEmptyResponseToNotification(t *testing.T) {
	tr, _ := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	ctx := testContext(t)
	if err := tr.Write(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Nothing should arrive.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if frame, err := tr.Read(short); err == nil {
		t.Errorf("a notification produced a frame: %s", frame)
	}
}

// The session id arrives with the initialize response and must be echoed on
// every request after it, or the server treats the next one as a stranger.
func TestHTTPEchoesSessionIDAfterInitialize(t *testing.T) {
	tr, recorder := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(headerSessionID, "session-abc")
		w.Header().Set("Content-Type", contentJSON)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
	})

	ctx := testContext(t)
	if err := tr.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := tr.Read(ctx); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if tr.SessionID() != "session-abc" {
		t.Fatalf("SessionID() = %q", tr.SessionID())
	}

	if err := tr.Write(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)); err != nil {
		t.Fatalf("second write: %v", err)
	}

	seen := recorder.seen()
	if len(seen) < 2 {
		t.Fatalf("server saw %d requests, want 2", len(seen))
	}
	if got := seen[1].Header.Get(headerSessionID); got != "session-abc" {
		t.Errorf("second request carried session id %q", got)
	}

	// The version header belongs on requests after initialization, and the
	// value is the one the server settled on.
	if got := seen[1].Header.Get(headerProtocolVersion); got != "2025-06-18" {
		t.Errorf("second request carried version %q, want 2025-06-18", got)
	}
	if got := seen[0].Header.Get(headerProtocolVersion); got != "" {
		t.Errorf("initialize carried a version header %q before one was agreed", got)
	}
}

func TestHTTPSendsBothAcceptTypes(t *testing.T) {
	tr, recorder := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	if err := tr.Write(testContext(t), []byte(`{"jsonrpc":"2.0","method":"x"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	accept := recorder.seen()[0].Header.Get("Accept")
	for _, want := range []string{contentJSON, contentSSE} {
		if !strings.Contains(accept, want) {
			t.Errorf("Accept = %q, missing %q", accept, want)
		}
	}
	if ct := recorder.seen()[0].Header.Get("Content-Type"); ct != contentJSON {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestHTTPReportsServerErrors(t *testing.T) {
	tr, _ := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "missing api key")
	})

	err := tr.Write(testContext(t), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err == nil {
		t.Fatal("Write hid a 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "missing api key") {
		t.Errorf("error = %v, want the status and the server's explanation", err)
	}
}

func TestHTTPSendsConfiguredHeaders(t *testing.T) {
	recorder := &recordingServer{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}}
	server := httptest.NewServer(recorder)
	defer server.Close()

	tr, err := NewHTTP(HTTPConfig{
		Endpoint: server.URL,
		Headers:  map[string]string{"Authorization": "Bearer token"},
	})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}
	defer tr.Close()

	if err := tr.Write(testContext(t), []byte(`{"jsonrpc":"2.0","method":"x"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := recorder.seen()[0].Header.Get("Authorization"); got != "Bearer token" {
		t.Errorf("Authorization = %q", got)
	}
}

// A session the server still believes is open holds resources on the far end,
// so closing says so explicitly.
func TestHTTPDeletesSessionOnClose(t *testing.T) {
	deleted := make(chan string, 1)

	recorder := &recordingServer{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted <- r.Header.Get(headerSessionID)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set(headerSessionID, "session-xyz")
		w.Header().Set("Content-Type", contentJSON)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}}
	server := httptest.NewServer(recorder)
	defer server.Close()

	tr, err := NewHTTP(HTTPConfig{Endpoint: server.URL})
	if err != nil {
		t.Fatalf("NewHTTP: %v", err)
	}

	ctx := testContext(t)
	if err := tr.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := tr.Read(ctx); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case id := <-deleted:
		if id != "session-xyz" {
			t.Errorf("DELETE carried session id %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("Close did not tell the server the session ended")
	}
}

func TestHTTPWriteAfterCloseFails(t *testing.T) {
	tr, _ := newHTTPTransport(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := tr.Write(testContext(t), []byte(`{}`)); err != ErrClosed {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
}

func TestNewHTTPRejectsEmptyEndpoint(t *testing.T) {
	if _, err := NewHTTP(HTTPConfig{}); err == nil {
		t.Fatal("NewHTTP accepted an empty endpoint")
	}
}
