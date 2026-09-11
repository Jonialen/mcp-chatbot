package transport

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// stderrLineLimit caps how much of a single stderr line is kept. Server logs
// are diagnostics, not protocol, so truncating one is harmless.
const stderrLineLimit = 1 << 20

// shutdownGrace is how long a server gets to exit on its own after its stdin is
// closed, before it is killed.
const shutdownGrace = 3 * time.Second

// Stdio speaks the MCP stdio transport: the server runs as a child process,
// frames go to its stdin one per line, and frames come back from its stdout the
// same way.
type Stdio struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	frames chan []byte
	done   chan struct{}

	onStderr func(line string)
	onNotice func(line string)

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool

	errOnce sync.Once
	readErr error
}

// StdioConfig describes the child process to launch.
type StdioConfig struct {
	// Command and Args are the executable and its arguments, exactly as they
	// would appear in a shell.
	Command string
	Args    []string

	// Env holds extra KEY=VALUE entries appended to the current environment.
	Env []string

	// Dir is the working directory of the child process. Empty means inherit.
	Dir string

	// OnStderr receives each line the server writes to stderr. MCP servers use
	// stderr for their own logging, so this is where their diagnostics surface.
	OnStderr func(line string)

	// OnNotice receives transport-level warnings, such as a line on stdout that
	// was not a JSON-RPC frame.
	OnNotice func(line string)
}

// NewStdio launches the server and starts pumping its output.
func NewStdio(cfg StdioConfig) (*Stdio, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("transport: no command given")
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("transport: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("transport: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("transport: stderr pipe: %w", err)
	}

	s := &Stdio{
		cmd:      cmd,
		stdin:    stdin,
		frames:   make(chan []byte),
		done:     make(chan struct{}),
		onStderr: cfg.OnStderr,
		onNotice: cfg.OnNotice,
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("transport: start %q: %w", cfg.Command, err)
	}

	go s.readFrames(stdout)
	go s.drainStderr(stderr)

	return s, nil
}

// Command returns the command line the server was launched with, for logging.
func (s *Stdio) Command() string {
	return s.cmd.String()
}

func (s *Stdio) Write(ctx context.Context, frame []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	// Serialize frames without holding the state lock across a pipe write.
	// Close must be able to close stdin even when the child stops reading it.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// The stdio transport is newline-delimited, so a frame is written together
	// with its terminator in a single call. Building the buffer here rather
	// than appending to the caller's slice keeps the caller's backing array
	// untouched.
	buf := make([]byte, 0, len(frame)+1)
	buf = append(buf, frame...)
	buf = append(buf, '\n')

	if _, err := s.stdin.Write(buf); err != nil {
		return fmt.Errorf("transport: write frame: %w", err)
	}
	return nil
}

func (s *Stdio) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case frame, ok := <-s.frames:
		if !ok {
			return nil, s.err()
		}
		return frame, nil
	}
}

func (s *Stdio) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	close(s.done)

	// Closing stdin is the polite shutdown signal: a well-behaved MCP server
	// sees EOF on its input and exits.
	_ = s.stdin.Close()

	exited := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(exited)
	}()

	select {
	case <-exited:
	case <-time.After(shutdownGrace):
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-exited
	}
	return nil
}

// readFrames turns the server's stdout into a stream of JSON-RPC frames.
func (s *Stdio) readFrames(stdout io.Reader) {
	defer close(s.frames)

	// bufio.Reader.ReadBytes grows without a fixed ceiling, unlike
	// bufio.Scanner, whose default 64 KiB token limit would reject a large
	// tool result such as the contents of a file.
	reader := bufio.NewReader(stdout)

	for {
		line, err := reader.ReadBytes('\n')

		if frame := bytes.TrimSpace(line); len(frame) > 0 {
			// Only well-formed frames are forwarded. Servers occasionally print
			// a banner or a stray warning to stdout, and one such line must not
			// be allowed to tear down an otherwise healthy session.
			if frame[0] == '{' || frame[0] == '[' {
				select {
				case s.frames <- frame:
				case <-s.done:
					s.setErr(ErrClosed)
					return
				}
			} else {
				s.notice(fmt.Sprintf("ignored non-JSON line on stdout: %s", truncate(frame)))
			}
		}

		if err != nil {
			s.setErr(fmt.Errorf("transport: read frame: %w", err))
			return
		}
	}
}

// drainStderr consumes the server's stderr for as long as it lives.
//
// This goroutine is not optional. An MCP server logs to stderr, and if nothing
// reads that pipe the operating system's buffer fills up, the server blocks
// forever on its next write, and the session hangs waiting for a response that
// will never be sent. The symptom looks like a protocol deadlock and is not one.
func (s *Stdio) drainStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), stderrLineLimit)
	for scanner.Scan() {
		if s.onStderr != nil {
			s.onStderr(scanner.Text())
		}
	}
}

func (s *Stdio) notice(line string) {
	if s.onNotice != nil {
		s.onNotice(line)
	}
}

func (s *Stdio) setErr(err error) {
	s.errOnce.Do(func() { s.readErr = err })
}

func (s *Stdio) err() error {
	if s.readErr != nil {
		return s.readErr
	}
	return io.EOF
}

func truncate(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
