package config

import (
	"bytes"
	"io"
)

// newTrimReader strips a UTF-8 byte order mark.
//
// A configuration file edited on Windows may begin with one, and the JSON
// decoder rejects it with an error that says nothing about its cause.
func newTrimReader(raw []byte) io.Reader {
	return bytes.NewReader(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
}
