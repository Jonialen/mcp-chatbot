package transport

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type shutdownRoundTripper func(*http.Request) (*http.Response, error)

func (f shutdownRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This body models an idle network read that only request cancellation releases.
type shutdownBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (b *shutdownBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*shutdownBody) Close() error { return nil }

func TestHTTPCloseCancelsIdleResponse(t *testing.T) {
	for _, contentType := range []string{contentSSE, contentJSON} {
		t.Run(contentType, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			tr, err := NewHTTP(HTTPConfig{Endpoint: "http://synthetic.invalid", Client: &http.Client{
				Transport: shutdownRoundTripper(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}},
						Body: &shutdownBody{ctx: r.Context(), started: started}}, nil
				}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			written := make(chan error, 1)
			go func() { written <- tr.Write(ctx, []byte(`{}`)) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("body read never started")
			}
			closed := make(chan error, 1)
			go func() { closed <- tr.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(time.Second):
				t.Error("Close blocked on an idle response")
				cancel() // Release the old implementation too; never leave a test goroutine behind.
				<-closed
			}
			select {
			case <-written:
			case <-time.After(time.Second):
				t.Error("Close left Write running")
				cancel()
				<-written
			}
			if _, err := tr.Read(context.Background()); err != ErrClosed {
				t.Errorf("Read after close: %v", err)
			}
		})
	}
}

func TestHTTPDeleteHasShutdownDeadline(t *testing.T) {
	var deadlineSet bool
	tr, err := NewHTTP(HTTPConfig{Endpoint: "http://synthetic.invalid", Client: &http.Client{
		Transport: shutdownRoundTripper(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			deadlineSet = ok && time.Until(deadline) > 0 && time.Until(deadline) <= shutdownGrace
			return nil, context.DeadlineExceeded
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	tr.setSessionID("synthetic")
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if !deadlineSet {
		t.Fatal("session DELETE can wait indefinitely: no shutdown deadline")
	}
}

func TestHTTPShutdownBoundsBlockedDelete(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			tr, err := NewHTTP(HTTPConfig{Endpoint: "http://synthetic.invalid", Headers: map[string]string{"Authorization": "synthetic"}, Client: &http.Client{
				Transport: shutdownRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodDelete || r.Header.Get("Authorization") != "synthetic" {
						t.Error("cleanup did not preserve method/authentication")
					}
					if phase == "headers" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &shutdownBody{
						ctx: r.Context(), started: make(chan struct{}),
					}}, nil
				}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			tr.setSessionID("synthetic")
			started := time.Now()
			if err := tr.Close(); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed < shutdownGrace || elapsed > shutdownGrace+time.Second {
				t.Fatalf("cleanup deadline not respected: %s", elapsed)
			}
		})
	}
}

func TestHTTPCloseCancelsPendingPost(t *testing.T) {
	started := make(chan struct{})
	tr, err := NewHTTP(HTTPConfig{Endpoint: "http://synthetic.invalid", Client: &http.Client{
		Transport: shutdownRoundTripper(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	written := make(chan error, 1)
	go func() { written <- tr.Write(ctx, []byte(`{}`)) }()
	<-started
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("pending POST succeeded after close")
		}
	case <-time.After(time.Second):
		cancel()
		<-written
		t.Fatal("pending POST survived close")
	}
}

type blockedStdin struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (w *blockedStdin) Write([]byte) (int, error) {
	close(w.started)
	<-w.closed
	return 0, io.ErrClosedPipe
}
func (w *blockedStdin) Close() error { w.once.Do(func() { close(w.closed) }); return nil }

func TestStdioCloseInterruptsBlockedWrite(t *testing.T) {
	stdin := &blockedStdin{started: make(chan struct{}), closed: make(chan struct{})}
	// Wait on an unstarted command returns immediately; no process is launched.
	tr := &Stdio{cmd: exec.Command("unused"), stdin: stdin, done: make(chan struct{})}
	written := make(chan error, 1)
	go func() { written <- tr.Write(context.Background(), []byte(`{}`)) }()
	<-stdin.started
	closed := make(chan error, 1)
	go func() { closed <- tr.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("Close cannot reach stdin.Close while Write holds the state mutex")
		_ = stdin.Close()
		<-closed
	}
	if err := <-written; err == nil {
		t.Fatal("blocked write unexpectedly succeeded")
	}
}
