// Package mcplog records every JSON-RPC frame exchanged with an MCP server.
//
// The log is kept verbatim: frames are written exactly as they crossed the
// transport, so the record doubles as evidence of what the protocol actually
// looked like on the wire rather than a rendering of what this program believed
// it sent.
package mcplog

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
)

const timeFormat = "15:04:05.000"

// Logger serialises frame records from every session onto one writer.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New returns a Logger writing to w.
func New(w io.Writer) *Logger {
	return &Logger{w: w}
}

// For returns a frame logger bound to one server, suitable for passing to a
// session. Frames from every server land on the same writer, in the order they
// happened, which is what makes a combined session readable.
func (l *Logger) For(server string) jsonrpc.FrameLogger {
	return func(dir jsonrpc.Direction, frame []byte) {
		l.write(server, arrow(dir), summarise(frame), frame)
	}
}

// Event records something that is not a frame: a server's stderr line, a
// lifecycle step, a transport warning.
func (l *Logger) Event(server, text string) {
	l.write(server, "·", "", []byte(text))
}

func (l *Logger) write(server, marker, label string, body []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	stamp := time.Now().Format(timeFormat)
	if label != "" {
		fmt.Fprintf(l.w, "%s  %-12s %s %-24s %s\n", stamp, server, marker, label, body)
		return
	}
	fmt.Fprintf(l.w, "%s  %-12s %s %-24s %s\n", stamp, server, marker, "", body)
}

func arrow(dir jsonrpc.Direction) string {
	if dir == jsonrpc.Outbound {
		return "-->"
	}
	return "<--"
}

// summarise names a frame so a long log stays scannable. The full frame is
// printed regardless; this is only the label in front of it.
func summarise(frame []byte) string {
	var msg jsonrpc.Message
	if err := json.Unmarshal(frame, &msg); err != nil {
		return "unparsed"
	}

	switch {
	case msg.IsRequest():
		return fmt.Sprintf("request %s", msg.Method)
	case msg.IsNotification():
		return fmt.Sprintf("notify %s", msg.Method)
	case msg.Error != nil:
		return fmt.Sprintf("error id=%s", msg.ID)
	case msg.IsResponse():
		return fmt.Sprintf("result id=%s", msg.ID)
	default:
		return "unknown"
	}
}
