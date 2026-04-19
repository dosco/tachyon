package http1

import "tachyon/buf"

// Header is one (name, value) pair as spans into the source buffer.
type Header struct {
	Name, Value buf.Span
}

// Request is a parsed HTTP/1.1 request. All Spans point into the buffer the
// caller passed to Parse; they are valid only while that buffer is retained.
//
// The struct is intentionally fixed-size and contains no pointers to slices,
// which keeps it out of the GC scan for pooled Request objects.
type Request struct {
	// src is the buffer the Spans point into. Held here so helpers can turn
	// spans back into []byte without the caller passing the source around.
	src []byte

	Method buf.Span
	Path   buf.Span

	Minor uint8 // 0 or 1 for HTTP/1.0 or HTTP/1.1

	// Parsed control headers. These are derived during the header loop so
	// the proxy doesn't need to rescan.
	ContentLength int64 // -1 = unknown
	Chunked       bool
	Close         bool // Connection: close or HTTP/1.0 without keep-alive

	NumHeaders int
	Headers    [MaxHeaders]Header
}

// Reset returns the request to its zero state without freeing the underlying
// Headers array. Call between reuses.
func (r *Request) Reset() {
	r.src = nil
	r.Method = buf.Span{}
	r.Path = buf.Span{}
	r.Minor = 0
	r.ContentLength = -1
	r.Chunked = false
	r.Close = false
	// Zero only the slots we used; the rest are already zero.
	for i := 0; i < r.NumHeaders; i++ {
		r.Headers[i] = Header{}
	}
	r.NumHeaders = 0
}

// Src returns the backing buffer the request's Spans point into. Callers
// that iterate Headers directly use this to resolve Span -> []byte.
func (r *Request) Src() []byte { return r.src }

// MethodBytes returns the method as a sub-slice of the parsed buffer.
func (r *Request) MethodBytes() []byte { return r.Method.Bytes(r.src) }

// PathBytes returns the request target as a sub-slice of the parsed buffer.
func (r *Request) PathBytes() []byte { return r.Path.Bytes(r.src) }

// Lookup returns the first header whose name case-folds equal to want, or
// nil if none. O(n) scan; fine for the small header counts HTTP/1 sees.
func (r *Request) Lookup(want []byte) []byte {
	for i := 0; i < r.NumHeaders; i++ {
		h := &r.Headers[i]
		if EqualFold(h.Name.Bytes(r.src), want) {
			return h.Value.Bytes(r.src)
		}
	}
	return nil
}
