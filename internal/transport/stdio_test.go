package transport

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// cat echoes stdin to stdout unchanged, which makes it a perfect stand-in for a
// server that answers every frame.
func newEchoTransport(t *testing.T, cfg StdioConfig) *Stdio {
	t.Helper()
	if cfg.Command == "" {
		cfg.Command = "cat"
	}
	tr, err := NewStdio(cfg)
	if err != nil {
		t.Fatalf("NewStdio: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestStdioRoundTrip(t *testing.T) {
	ctx := testContext(t)
	tr := newEchoTransport(t, StdioConfig{})

	sent := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if err := tr.Write(ctx, sent); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := tr.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(sent) {
		t.Errorf("Read() = %s, want %s", got, sent)
	}
}

// A tool result carrying a file easily exceeds bufio.Scanner's default 64 KiB
// token limit. Frames are read with bufio.Reader precisely so that a large
// result is not silently rejected.
func TestStdioHandlesFrameLargerThanScannerDefault(t *testing.T) {
	ctx := testContext(t)
	tr := newEchoTransport(t, StdioConfig{})

	payload := strings.Repeat("a", 512*1024)
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]string{"text": payload},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := tr.Write(ctx, frame); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := tr.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(frame) {
		t.Fatalf("Read() returned %d bytes, want %d", len(got), len(frame))
	}
}

// One stray banner on stdout must not take the session down: the line is
// reported and skipped, and the frame after it still arrives.
func TestStdioSkipsNonJSONLinesOnStdout(t *testing.T) {
	ctx := testContext(t)

	var mu sync.Mutex
	var notices []string

	tr := newEchoTransport(t, StdioConfig{
		Command: "sh",
		Args:    []string{"-c", "echo 'my-server v1.0 starting up'; exec cat"},
		OnNotice: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			notices = append(notices, line)
		},
	})

	sent := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	if err := tr.Write(ctx, sent); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := tr.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(sent) {
		t.Errorf("Read() = %s, want the frame that followed the banner", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(notices) == 0 {
		t.Error("skipped line was not reported through OnNotice")
	}
}

// Server logs arrive on stderr and must be drained, or the server blocks on a
// full pipe and the session deadlocks.
func TestStdioDrainsStderr(t *testing.T) {
	ctx := testContext(t)

	lines := make(chan string, 4)
	tr := newEchoTransport(t, StdioConfig{
		Command:  "sh",
		Args:     []string{"-c", "echo 'server ready' >&2; exec cat"},
		OnStderr: func(line string) { lines <- line },
	})

	select {
	case got := <-lines:
		if got != "server ready" {
			t.Errorf("OnStderr got %q", got)
		}
	case <-ctx.Done():
		t.Fatal("stderr line never surfaced")
	}

	// The session still works after the log line.
	sent := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	if err := tr.Write(ctx, sent); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := tr.Read(ctx); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

func TestStdioReadReturnsErrorAfterServerExits(t *testing.T) {
	ctx := testContext(t)
	tr := newEchoTransport(t, StdioConfig{
		Command: "sh",
		Args:    []string{"-c", "exit 0"},
	})

	if _, err := tr.Read(ctx); err == nil {
		t.Fatal("Read() succeeded after the server exited, want an error")
	}
}

func TestStdioWriteAfterCloseFails(t *testing.T) {
	ctx := testContext(t)
	tr := newEchoTransport(t, StdioConfig{})

	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close must be safe to call twice.
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := tr.Write(ctx, []byte(`{}`)); err != ErrClosed {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
}
