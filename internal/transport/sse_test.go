package transport

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReaderDecodesEvents(t *testing.T) {
	stream := strings.Join([]string{
		": a comment used as a keep-alive",
		"",
		"event: message",
		`data: {"jsonrpc":"2.0","id":1,"result":{}}`,
		"",
		"id: 42",
		"event: message",
		`data: {"jsonrpc":"2.0","id":2,"result":{}}`,
		"",
	}, "\n")

	reader := newSSEReader(strings.NewReader(stream))

	first, err := reader.next()
	if err != nil {
		t.Fatalf("first event: %v", err)
	}
	if string(first) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Errorf("first payload = %s", first)
	}

	second, err := reader.next()
	if err != nil {
		t.Fatalf("second event: %v", err)
	}
	if string(second) != `{"jsonrpc":"2.0","id":2,"result":{}}` {
		t.Errorf("second payload = %s", second)
	}

	if _, err := reader.next(); err != io.EOF {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
}

// A frame split across several data lines rejoins with newlines.
func TestSSEReaderJoinsMultilineData(t *testing.T) {
	reader := newSSEReader(strings.NewReader("data: {\ndata: \"a\": 1\ndata: }\n\n"))

	payload, err := reader.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(payload) != "{\n\"a\": 1\n}" {
		t.Errorf("payload = %q", payload)
	}
}

func TestSSEReaderHandlesCarriageReturns(t *testing.T) {
	reader := newSSEReader(strings.NewReader("data: {\"a\":1}\r\n\r\n"))

	payload, err := reader.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(payload) != `{"a":1}` {
		t.Errorf("payload = %q", payload)
	}
}

// A server that closes without a trailing blank line still delivers its last
// event.
func TestSSEReaderReturnsUnterminatedFinalEvent(t *testing.T) {
	reader := newSSEReader(strings.NewReader(`data: {"a":1}`))

	payload, err := reader.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(payload) != `{"a":1}` {
		t.Errorf("payload = %q", payload)
	}
}

// Keep-alives carry no data and must not surface as empty frames.
func TestSSEReaderSkipsKeepAlives(t *testing.T) {
	reader := newSSEReader(strings.NewReader(": ping\n\n: ping\n\ndata: {\"a\":1}\n\n"))

	payload, err := reader.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if string(payload) != `{"a":1}` {
		t.Errorf("payload = %q, want the keep-alives skipped", payload)
	}
}
