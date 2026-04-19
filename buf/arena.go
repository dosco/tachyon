package buf

// Span points at a substring of the arena's backing buffer.
//
// We use (off, len) instead of a []byte because a []byte is 24 bytes (ptr,
// len, cap) and has pointer semantics that interact with escape analysis. A
// Span is 8 bytes and trivially stack-allocatable. The parser produces
// thousands of these per connection; that matters.
type Span struct {
	Off uint32
	Len uint32
}

// Bytes returns the span's content as a sub-slice of src. The returned slice
// aliases src and is only valid while the backing buffer is retained.
func (s Span) Bytes(src []byte) []byte {
	return src[s.Off : s.Off+s.Len]
}

// Empty reports whether the span has zero length.
func (s Span) Empty() bool { return s.Len == 0 }

// Arena is an append-only writer over a caller-owned byte buffer. The HTTP/1
// parser uses it to pack header names and values into the same underlying
// buffer it recv'd into, so hand-outs from the arena are just Spans.
//
// Arena itself owns no memory. It is reset by pointing it at new backing
// storage, not by allocating.
type Arena struct {
	buf []byte
	n   uint32
}

// Reset makes b the arena's backing buffer and rewinds the write cursor. The
// arena aliases b; the caller retains ownership.
func (a *Arena) Reset(b []byte) {
	a.buf = b
	a.n = 0
}

// Len returns the number of bytes written.
func (a *Arena) Len() int { return int(a.n) }

// Cap returns the total backing capacity.
func (a *Arena) Cap() int { return len(a.buf) }

// Put appends p and returns a Span that addresses it. If the arena is full,
// Put returns an empty Span and the caller must decide whether to spill. We
// never panic: a proxy must handle overflow as a protocol error, not a crash.
func (a *Arena) Put(p []byte) Span {
	if int(a.n)+len(p) > len(a.buf) {
		return Span{}
	}
	off := a.n
	copy(a.buf[off:], p)
	a.n += uint32(len(p))
	return Span{Off: off, Len: uint32(len(p))}
}

// PutByte appends a single byte. Useful for building a separator without
// allocating a [1]byte on the heap.
func (a *Arena) PutByte(c byte) Span {
	if int(a.n)+1 > len(a.buf) {
		return Span{}
	}
	off := a.n
	a.buf[off] = c
	a.n++
	return Span{Off: off, Len: 1}
}
