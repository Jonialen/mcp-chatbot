package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/transport"
)

// fakeTransport stands in for a server: frames written by the client land in
// written, and frames pushed into incoming are handed to the client's reader.
type fakeTransport struct {
	written  chan []byte
	incoming chan []byte
	closed   chan struct{}
	once     sync.Once
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		written:  make(chan []byte, 16),
		incoming: make(chan []byte, 16),
		closed:   make(chan struct{}),
	}
}

func (f *fakeTransport) Write(ctx context.Context, frame []byte) error {
	dup := make([]byte, len(frame))
	copy(dup, frame)
	select {
	case f.written <- dup:
		return nil
	case <-f.closed:
		return transport.ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeTransport) Read(ctx context.Context) ([]byte, error) {
	select {
	case frame := <-f.incoming:
		return frame, nil
	case <-f.closed:
		return nil, transport.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeTransport) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTransport) push(t *testing.T, frame string) {
	t.Helper()
	select {
	case f.incoming <- []byte(frame):
	case <-time.After(2 * time.Second):
		t.Fatal("pushing an inbound frame timed out")
	}
}

func (f *fakeTransport) nextWritten(t *testing.T) Message {
	t.Helper()
	select {
	case frame := <-f.written:
		var msg Message
		if err := json.Unmarshal(frame, &msg); err != nil {
			t.Fatalf("client wrote an unparseable frame %s: %v", frame, err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("client wrote no frame")
		return Message{}
	}
}

func newTestClient(t *testing.T, cfg ClientConfig) (*Client, *fakeTransport) {
	t.Helper()
	tr := newFakeTransport()
	cfg.Transport = tr

	client := NewClient(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = client.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		_ = client.Close()
	})
	return client, tr
}

// The point of the pending map: three calls are in flight at once and the
// server answers them in reverse order. Each caller must still get its own
// response and nobody else's.
func TestCallsCorrelateOutOfOrderResponses(t *testing.T) {
	client, tr := newTestClient(t, ClientConfig{})
	ctx := context.Background()

	type outcome struct {
		method string
		result string
		err    error
	}
	results := make(chan outcome, 3)
	methods := []string{"alpha", "beta", "gamma"}

	for _, method := range methods {
		go func() {
			raw, err := client.Call(ctx, method, nil)
			results <- outcome{method: method, result: string(raw), err: err}
		}()
	}

	// Collect the three requests, then answer them backwards.
	ids := make(map[string]json.RawMessage, len(methods))
	for range methods {
		msg := tr.nextWritten(t)
		ids[msg.Method] = msg.ID
	}
	for i := len(methods) - 1; i >= 0; i-- {
		method := methods[i]
		tr.push(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"from":%q}}`, ids[method], method))
	}

	seen := make(map[string]string, len(methods))
	for range methods {
		select {
		case out := <-results:
			if out.err != nil {
				t.Fatalf("Call(%s): %v", out.method, out.err)
			}
			seen[out.method] = out.result
		case <-time.After(3 * time.Second):
			t.Fatal("a call never received its response")
		}
	}

	for _, method := range methods {
		want := fmt.Sprintf(`{"from":%q}`, method)
		if seen[method] != want {
			t.Errorf("Call(%s) got %s, want %s", method, seen[method], want)
		}
	}
}

func TestCallReturnsProtocolError(t *testing.T) {
	client, tr := newTestClient(t, ClientConfig{})

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "tools/list", nil)
		done <- err
	}()

	msg := tr.nextWritten(t)
	tr.push(t, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unknown method"}}`, msg.ID))

	err := <-done
	if !IsMethodNotFound(err) {
		t.Fatalf("Call() error = %v, want a method-not-found JSON-RPC error", err)
	}

	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is not an *Error: %v", err)
	}
	if rpcErr.Message != "unknown method" {
		t.Errorf("message = %q", rpcErr.Message)
	}
}

// A server that asks us something we cannot do must get an answer, not silence.
// Silence leaves it blocked on a response that is never coming.
func TestServerRequestIsRefusedNotIgnored(t *testing.T) {
	_, tr := newTestClient(t, ClientConfig{})

	tr.push(t, `{"jsonrpc":"2.0","id":99,"method":"sampling/createMessage","params":{}}`)

	msg := tr.nextWritten(t)
	if string(msg.ID) != "99" {
		t.Errorf("reply id = %s, want the id the server sent (99)", msg.ID)
	}
	if msg.Error == nil {
		t.Fatalf("reply carried no error object: %+v", msg)
	}
	if msg.Error.Code != CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", msg.Error.Code, CodeMethodNotFound)
	}
}

func TestNotificationsReachTheHandler(t *testing.T) {
	got := make(chan string, 1)
	_, tr := newTestClient(t, ClientConfig{
		OnNotification: func(method string, _ json.RawMessage) { got <- method },
	})

	tr.push(t, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)

	select {
	case method := <-got:
		if method != "notifications/tools/list_changed" {
			t.Errorf("handler got %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notification never reached the handler")
	}

	// A notification must not be answered.
	select {
	case frame := <-tr.written:
		t.Errorf("client replied to a notification: %s", frame)
	case <-time.After(200 * time.Millisecond):
	}
}

// Servers are required to echo the id unchanged, but a few re-serialise it.
// A response must still find its caller.
func TestResponseMatchesDespiteReserialisedID(t *testing.T) {
	for _, echoed := range []string{`1`, `1.0`, `"1"`} {
		t.Run("id "+echoed, func(t *testing.T) {
			client, tr := newTestClient(t, ClientConfig{})

			done := make(chan error, 1)
			go func() {
				_, err := client.Call(context.Background(), "ping", nil)
				done <- err
			}()

			tr.nextWritten(t)
			tr.push(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, echoed))

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Call() = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("response with id %s never matched its caller", echoed)
			}
		})
	}
}

// One unparseable frame must not end the session.
func TestGarbageFrameDoesNotKillSession(t *testing.T) {
	client, tr := newTestClient(t, ClientConfig{})

	tr.push(t, `this is not json at all`)

	done := make(chan error, 1)
	go func() {
		_, err := client.Call(context.Background(), "ping", nil)
		done <- err
	}()

	msg := tr.nextWritten(t)
	tr.push(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Call() after a garbage frame = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session died on a malformed frame")
	}
}

func TestCallReleasedOnContextCancel(t *testing.T) {
	client, tr := newTestClient(t, ClientConfig{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "slow", nil)
		done <- err
	}()

	tr.nextWritten(t)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Call() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the context did not release the call")
	}
}

func TestFramesAreLoggedInBothDirections(t *testing.T) {
	var mu sync.Mutex
	var seen []Direction

	client, tr := newTestClient(t, ClientConfig{
		LogFrame: func(dir Direction, _ []byte) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, dir)
		},
	})

	done := make(chan struct{})
	go func() {
		_, _ = client.Call(context.Background(), "ping", nil)
		close(done)
	}()

	msg := tr.nextWritten(t)
	tr.push(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID))
	<-done

	mu.Lock()
	defer mu.Unlock()
	var out, in int
	for _, dir := range seen {
		switch dir {
		case Outbound:
			out++
		case Inbound:
			in++
		}
	}
	if out == 0 || in == 0 {
		t.Errorf("logged %d outbound and %d inbound frames, want both", out, in)
	}
}
