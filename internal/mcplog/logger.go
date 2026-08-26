// Package mcplog records every JSON-RPC frame exchanged with an MCP server.
//
// The record is kept verbatim, so it is evidence of what the protocol actually
// looked like on the wire rather than a rendering of what this program believed
// it sent. That file is the deliverable; the screen is a convenience.
//
// Screen and file are separate on purpose. A single tools/list result runs to
// thirteen kilobytes, which is worth keeping and unreadable to watch scroll
// past, so the screen gets a one-line summary unless verbose output is asked
// for.
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

// screenFrameLimit caps how much of a frame is shown on screen in verbose mode.
const screenFrameLimit = 2000

// Logger serialises frame records from every session.
type Logger struct {
	mu      sync.Mutex
	file    io.Writer
	screen  io.Writer
	verbose bool
	frames  int
}

// Options configures a Logger.
type Options struct {
	// File receives every frame in full. It is the durable record.
	File io.Writer

	// Screen, if set, receives a summary of every frame as it happens.
	Screen io.Writer

	// Verbose makes the screen show whole frames instead of summaries.
	Verbose bool
}

// New returns a Logger.
func New(opts Options) *Logger {
	return &Logger{file: opts.File, screen: opts.Screen, verbose: opts.Verbose}
}

// SetVerbose switches the screen between summaries and whole frames. The file
// is unaffected: it always holds everything.
func (l *Logger) SetVerbose(verbose bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verbose = verbose
}

// Verbose reports whether the screen is showing whole frames.
func (l *Logger) Verbose() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.verbose
}

// Frames reports how many frames have been recorded, for the session summary.
func (l *Logger) Frames() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.frames
}

// For returns a frame logger bound to one server, suitable for passing to a
// session. Every server's frames land in the same record, in the order they
// happened, which is what makes a multi-server session readable.
func (l *Logger) For(server string) jsonrpc.FrameLogger {
	return func(dir jsonrpc.Direction, frame []byte) {
		l.record(server, arrow(dir), summarise(frame), frame)
	}
}

// Event records something that is not a frame: a server's stderr line, a
// lifecycle step, a transport warning.
func (l *Logger) Event(server, text string) {
	l.record(server, " · ", "", []byte(text))
}

func (l *Logger) record(server, marker, label string, frame []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.frames++
	stamp := time.Now().Format(timeFormat)

	if l.file != nil {
		fmt.Fprintf(l.file, "%s  %-14s %s %-26s %s\n", stamp, server, marker, label, frame)
	}
	if l.screen == nil {
		return
	}

	body := label
	if l.verbose || label == "" {
		body = truncate(frame, screenFrameLimit)
	}
	fmt.Fprintf(l.screen, "  %s  %-14s %s %s\n", stamp, server, marker, body)
}

func arrow(dir jsonrpc.Direction) string {
	if dir == jsonrpc.Outbound {
		return "-->"
	}
	return "<--"
}

// summarise names a frame so a long record stays scannable. The frame itself is
// always written to the file regardless; this is only the label in front of it.
func summarise(frame []byte) string {
	var msg jsonrpc.Message
	if err := json.Unmarshal(frame, &msg); err != nil {
		return "unparsed frame"
	}

	switch {
	case msg.IsRequest():
		return "request " + msg.Method
	case msg.IsNotification():
		return "notify " + msg.Method
	case msg.Error != nil:
		return fmt.Sprintf("error id=%s", msg.ID)
	case msg.IsResponse():
		return fmt.Sprintf("result id=%s (%d bytes)", msg.ID, len(frame))
	default:
		return "unknown frame"
	}
}

func truncate(frame []byte, max int) string {
	if len(frame) <= max {
		return string(frame)
	}
	return fmt.Sprintf("%s... (%d bytes total)", frame[:max], len(frame))
}
