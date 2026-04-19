package http1

import "tachyon/buf"

// Response is a parsed HTTP/1.1 response. Like Request, all Spans point into
// the buffer the caller passed to ParseResponse.
type Response struct {
	src []byte

	Minor    uint8
	Status   uint16
	Reason   buf.Span

	ContentLength int64
	Chunked       bool
	Close         bool

	NumHeaders int
	Headers    [MaxHeaders]Header
}

// Reset returns the response to its zero state for reuse.
func (r *Response) Reset() {
	r.src = nil
	r.Minor = 0
	r.Status = 0
	r.Reason = buf.Span{}
	r.ContentLength = -1
	r.Chunked = false
	r.Close = false
	for i := 0; i < r.NumHeaders; i++ {
		r.Headers[i] = Header{}
	}
	r.NumHeaders = 0
}

// Src returns the backing buffer the response's Spans point into.
func (r *Response) Src() []byte { return r.src }

// ReasonBytes returns the status reason phrase.
func (r *Response) ReasonBytes() []byte { return r.Reason.Bytes(r.src) }

// Lookup returns the first header whose name case-folds equal to want.
func (r *Response) Lookup(want []byte) []byte {
	for i := 0; i < r.NumHeaders; i++ {
		h := &r.Headers[i]
		if EqualFold(h.Name.Bytes(r.src), want) {
			return h.Value.Bytes(r.src)
		}
	}
	return nil
}
