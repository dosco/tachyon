//go:build linux

// Package-level H3 handler: HTTP/3 request → H1 upstream → HTTP/3 response. Reuses
// the H2 handler's ServeH2 by adapting QPACK fields into H2 header
// fields and wrapping the HTTP/3 response writer to the http2.ResponseWriter
// interface. Same intent/router/pool plumbing as H1 and H2.
package proxy

import (
	"bytes"
	"context"
	"io"

	"tachyon/http2"
	"tachyon/http3"
	"tachyon/http3/qpack"
)

// H3Handler adapts HTTP/3 requests to the same proxy pipeline as H2.
type H3Handler struct {
	h2 *H2Handler
}

// NewH3Handler wraps the parent H2 handler so both protocols share
// intent evaluation, routing, and upstream pools.
func NewH3Handler(parent *Handler) *H3Handler {
	return &H3Handler{h2: NewH2Handler(parent)}
}

// Handle is an http3.Handler compatible with http3.Serve.
func (h *H3Handler) Handle(_ context.Context, req *http3.Request, rw *http3.ResponseWriter) {
	fields := make([]http2.HeaderField, 0, len(req.Headers))
	for _, f := range req.Headers {
		fields = append(fields, http2.HeaderField{Name: f.Name, Value: f.Value})
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	w := &h3RespAdapter{rw: rw}
	_ = h.h2.ServeH2(req.Method, req.Path, req.Authority, fields, body, w)
}

// h3RespAdapter satisfies http2.ResponseWriter by driving
// http3.ResponseWriter. Status + headers are captured on WriteHeader;
// bodies accumulate in the underlying http3.ResponseWriter which
// writes HEADERS+DATA on stream close.
type h3RespAdapter struct {
	rw       *http3.ResponseWriter
	hdrSent  bool
}

func (a *h3RespAdapter) WriteHeader(status int, fields []http2.HeaderField) error {
	a.rw.Status = status
	for _, f := range fields {
		a.rw.Header = append(a.rw.Header, qpack.Field{Name: f.Name, Value: f.Value})
	}
	a.hdrSent = true
	return nil
}

func (a *h3RespAdapter) Write(p []byte) (int, error) {
	return a.rw.Write(p)
}

func (a *h3RespAdapter) Close() error { return nil }
