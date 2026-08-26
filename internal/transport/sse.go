package transport

import (
	"bufio"
	"bytes"
	"io"
)

// sseReader decodes a text/event-stream body into the payloads it carries.
//
// The format is line based: fields accumulate until a blank line dispatches the
// event. Only the data field matters here, because MCP carries whole JSON-RPC
// frames in it; comments, event names, ids and retry hints are read and skipped
// so a compliant stream is consumed correctly either way.
type sseReader struct {
	lines *bufio.Reader
}

func newSSEReader(body io.Reader) *sseReader {
	return &sseReader{lines: bufio.NewReader(body)}
}

// next returns the payload of the next event, or an error once the stream ends.
func (r *sseReader) next() ([]byte, error) {
	var data []byte

	for {
		line, err := r.lines.ReadBytes('\n')

		if len(line) > 0 {
			// A stream may use either line ending.
			field := bytes.TrimRight(line, "\r\n")

			switch {
			case len(field) == 0:
				// Blank line: the event is complete. An event with no data
				// field is a keep-alive and is skipped rather than returned.
				if len(data) > 0 {
					return data, nil
				}

			case field[0] == ':':
				// A comment, commonly used to keep the connection alive.

			default:
				name, value := splitField(field)
				if string(name) == "data" {
					// Several data lines in one event join with newlines.
					if len(data) > 0 {
						data = append(data, '\n')
					}
					data = append(data, value...)
				}
			}
		}

		if err != nil {
			// A stream that ends mid-event still delivers what it had, so a
			// server that closes without a trailing blank line is tolerated.
			if len(data) > 0 {
				return data, nil
			}
			return nil, err
		}
	}
}

// splitField separates a field name from its value, dropping the single
// optional space after the colon.
func splitField(line []byte) (name, value []byte) {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		// A line with no colon is a field name with an empty value.
		return line, nil
	}

	name = line[:colon]
	value = line[colon+1:]
	return name, bytes.TrimPrefix(value, []byte(" "))
}
