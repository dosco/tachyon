// Package http3 is the HTTP/3 server on top of tachyon/quic. Phase 4
// scope is request/response streams only — server push, the control-
// stream SETTINGS exchange, and dynamic-table QPACK are deferred.
//
// Each client-initiated bidirectional QUIC stream carries one HTTP/3
// request/response exchange. The wire layout is a sequence of HEADERS
// and DATA frames (RFC 9114 §4.1). HEADERS payloads are QPACK-encoded
// field sections.
package http3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"tachyon/http3/frame"
	"tachyon/http3/qpack"
	"tachyon/quic"
)

// Request is one parsed HTTP/3 request.
type Request struct {
	Method    string
	Scheme    string
	Authority string
	Path      string
	Headers   []qpack.Field // non-pseudo headers
	Body      []byte        // fully buffered for Phase 4; streaming comes with proxy integration
}

// ResponseWriter collects a response for a single HTTP/3 request. Call
// Write to append body bytes, then either Close() to FIN the stream or
// rely on the server to do so when the handler returns.
type ResponseWriter struct {
	Status  int
	Header  []qpack.Field
	Body    []byte
}

// Handler processes a single request. The server dispatches one Handler
// invocation per bidi stream.
type Handler func(context.Context, *Request, *ResponseWriter)

// Serve runs the HTTP/3 accept loop for cs, invoking h for each
// complete request it receives. Returns when ctx is done. Loop-level
// errors (malformed frames) close the stream but keep the connection.
func Serve(ctx context.Context, cs Connection, h Handler) error {
	for {
		s, err := cs.AcceptStream(ctx)
		if err != nil {
			return err
		}
		go serveStream(ctx, s, h, cs.Flush)
	}
}

// Connection is the minimum surface http3 needs from a QUIC connection.
// An interface lets tests substitute an in-memory fake later.
type Connection interface {
	AcceptStream(context.Context) (*quic.Stream, error)
	Flush() error
}

// ServeConn is a convenience wrapper that accepts a *quic.Conn and
// dispatches handlers. Identical to Serve but avoids making callers
// import the Connection interface name explicitly.
func ServeConn(ctx context.Context, c *quic.Conn, h Handler) error {
	return Serve(ctx, c, h)
}

func serveStream(ctx context.Context, s *quic.Stream, h Handler, flush func() error) {
	defer s.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	var fin bool
	for !fin {
		n, rfin := s.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if rfin {
			fin = true
			break
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.RecvSignal():
			}
		}
	}

	req, err := parseRequest(buf)
	if err != nil {
		writeError(s, 400)
		return
	}
	rw := &ResponseWriter{Status: 200}
	h(ctx, req, rw)
	if err := writeResponse(s, rw); err != nil {
		return
	}
	_ = s.CloseWrite()
	if flush != nil {
		_ = flush()
	}
}

// parseRequest walks the HEADERS+DATA stream and materializes a
// Request. Trailers (a second HEADERS frame after DATA) are collected
// into Request.Headers alongside the leading ones.
func parseRequest(buf []byte) (*Request, error) {
	r := &Request{}
	for len(buf) > 0 {
		f, n, err := frame.Parse(buf)
		if err != nil {
			return nil, err
		}
		buf = buf[n:]
		switch f.Type {
		case frame.TypeHeaders:
			fields, err := qpack.Decode(f.Payload)
			if err != nil {
				return nil, err
			}
			for _, fd := range fields {
				switch fd.Name {
				case ":method":
					r.Method = fd.Value
				case ":scheme":
					r.Scheme = fd.Value
				case ":authority":
					r.Authority = fd.Value
				case ":path":
					r.Path = fd.Value
				default:
					r.Headers = append(r.Headers, fd)
				}
			}
		case frame.TypeData:
			r.Body = append(r.Body, f.Payload...)
		default:
			// Unknown frames on a request stream are legal per RFC 9114
			// §7.2.8 (reserved/grease); skip and continue.
		}
	}
	if r.Method == "" {
		return nil, errors.New("http3: request missing :method")
	}
	return r, nil
}

func writeResponse(s *quic.Stream, rw *ResponseWriter) error {
	hdrs := []qpack.Field{{Name: ":status", Value: strconv.Itoa(rw.Status)}}
	hdrs = append(hdrs, rw.Header...)
	block := qpack.Encode(nil, hdrs)
	out := frame.AppendFrame(nil, frame.TypeHeaders, block)
	if len(rw.Body) > 0 {
		out = frame.AppendFrame(out, frame.TypeData, rw.Body)
	}
	if _, err := s.Write(out); err != nil {
		return err
	}
	return nil
}

func writeError(s *quic.Stream, status int) {
	rw := &ResponseWriter{Status: status, Body: []byte(fmt.Sprintf("status %d\n", status))}
	_ = writeResponse(s, rw)
	_ = s.CloseWrite()
}

// --- ResponseWriter helpers ---

// Write appends bytes to the response body.
func (rw *ResponseWriter) Write(p []byte) (int, error) {
	rw.Body = append(rw.Body, p...)
	return len(p), nil
}

// SetHeader appends a header field (case-sensitive name, per RFC 9113).
func (rw *ResponseWriter) SetHeader(name, value string) {
	rw.Header = append(rw.Header, qpack.Field{Name: name, Value: value})
}

var _ io.Writer = (*ResponseWriter)(nil)
