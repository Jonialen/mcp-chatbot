package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/Jonialen/mcp-chatbot/internal/jsonrpc"
)

// ServeStdio runs the server over the stdio transport, reading newline
// delimited frames from in and writing them to out.
//
// Nothing but frames may reach out. A server that prints a banner or a log line
// to its stdout corrupts the stream for its client, which is why diagnostics
// belong on stderr.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)

	// Writes are serialised because a handler may run on its own goroutine.
	var writeMu sync.Mutex
	write := func(msg *jsonrpc.Message) error {
		frame, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("encode frame: %w", err)
		}

		writeMu.Lock()
		defer writeMu.Unlock()

		if _, err := out.Write(append(frame, '\n')); err != nil {
			return fmt.Errorf("write frame: %w", err)
		}
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		line, err := reader.ReadBytes('\n')

		if frame := trimSpace(line); len(frame) > 0 {
			response := s.handleFrame(ctx, frame)
			if response != nil {
				if writeErr := write(response); writeErr != nil {
					return writeErr
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				// The client closed its end: an ordinary shutdown.
				return nil
			}
			return fmt.Errorf("read frame: %w", err)
		}
	}
}

// handleFrame decodes one frame and produces the reply, if any.
func (s *Server) handleFrame(ctx context.Context, frame []byte) *jsonrpc.Message {
	var msg jsonrpc.Message
	if err := json.Unmarshal(frame, &msg); err != nil {
		// A frame that will not parse carries no id to answer against, so the
		// error the specification defines for it uses a null id.
		return jsonrpc.NewErrorResponse(json.RawMessage("null"),
			jsonrpc.CodeParseError, "malformed JSON: "+err.Error())
	}
	return s.Handle(ctx, &msg)
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
