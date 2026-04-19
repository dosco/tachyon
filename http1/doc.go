// Package http1 is a zero-allocation HTTP/1.1 codec.
//
// It is intentionally not a net/http replacement. It provides only what a
// reverse proxy needs: a state-machine parser that slices into the caller's
// recv buffer (no copies, no strings), and a fixed-byte writer that serialises
// requests and responses into a caller-owned buffer.
//
// # The zero-alloc contract
//
// Parse does not allocate. Request.Method, .Path, .Headers[i].Name, .Headers[i].Value
// are [buf.Span] views into the recv buffer the caller passed to Parse. As
// long as the caller retains that buffer, the Request's views are valid.
//
// Writing does not allocate either: the writer appends to a caller-owned
// []byte (typically from a buf.Slab). Status codes are emitted from a
// pre-encoded table; Content-Length integers go through a stack itoa.
//
// # Layout
//
//   - constants.go       - pre-encoded status lines, common headers, methods
//   - header_scan.go     - ASCII helpers, token/value scanning, case-insensitive compare
//   - request.go         - Request struct and Reset
//   - request_parser.go  - request state machine
//   - response.go        - Response struct and Reset
//   - response_parser.go - response state machine
//   - response_writer.go - fixed-byte writer
//   - chunked.go         - chunked transfer codec
//
// # Error model
//
// Parse returns either (n, nil) for a complete message, (0, ErrNeedMore) if
// more bytes are required, or (0, some specific error) for a protocol
// violation. Specific errors let the caller decide whether to close the
// connection (malformed) or answer with 400/431 (oversized).
package http1

import "errors"

var (
	// ErrNeedMore says the buffer did not contain a complete message.
	// Callers read more bytes and call Parse again with the longer slice.
	ErrNeedMore = errors.New("http1: need more data")

	// ErrMalformed is a generic protocol error. Callers should close.
	ErrMalformed = errors.New("http1: malformed")

	// ErrTooLarge means the message exceeded our header budget. Callers
	// should answer 431 and close.
	ErrTooLarge = errors.New("http1: message too large")
)
