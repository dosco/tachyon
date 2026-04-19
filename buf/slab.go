package buf

import "sync/atomic"

// Class identifies a slab size class. Three classes cover the needs of an
// HTTP/1.1 proxy:
//
//   - ClassHeader (4 KiB): request/response write buffers; the upstream
//     request line plus rewritten headers comfortably fit.
//   - ClassRead   (16 KiB): inbound recv buffer; enough for pathological
//     header sets (Cookie chains, WAF prefixes) on keep-alive.
//   - ClassBody   (64 KiB): for plaintext body forwarding when we can't splice.
//
// Adding a class is cheap (one sync.Pool); removing one is expensive (churn in
// callers). Resist growing this list without a measured reason.
type Class uint8

const (
	ClassHeader Class = iota
	ClassRead
	ClassBody
	numClasses
)

// Size returns the byte capacity of slabs in this class.
func (c Class) Size() int {
	switch c {
	case ClassHeader:
		return 4 << 10
	case ClassRead:
		return 16 << 10
	case ClassBody:
		return 64 << 10
	}
	return 0
}

// Slab is a reusable byte buffer drawn from a size-classed pool. It remembers
// its class so Put can route it back to the correct pool.
//
// The `written` high-water mark lets Reset zero only the bytes that were
// actually used, not the whole slab. Callers that write into the slab call
// MarkWritten(n) with the highest byte offset they touched; Reset clears
// s.b[:written]. This keeps stale request bytes from leaking across
// successive Get/Put cycles — important on TLS where a fresh session
// could otherwise observe a previous session's plaintext header bytes —
// while avoiding a 16-KiB memclr on the bench hot path.
type Slab struct {
	class   Class
	b       []byte
	written int
}

// Bytes returns the underlying buffer with full capacity and length.
// Callers treat this as scratch space; it is the Slab's entire storage.
func (s *Slab) Bytes() []byte { return s.b }

// Class returns the size class for debugging and assertions.
func (s *Slab) Class() Class { return s.class }

// MarkWritten records that the caller wrote up to n bytes into Bytes().
// Reset uses this to bound the clear. The setter takes max(current, n)
// so a caller that tracks its own high-water across multiple writes can
// report each write safely; callers that report only the final extent
// at Put time pay the same.
//
// Zero-alloc; no syscalls.
func (s *Slab) MarkWritten(n int) {
	if n > s.written {
		s.written = n
	}
}

// Reset zeroes the written region of the slab, so the next caller sees
// all-zero bytes in that region and cannot observe the prior request's
// bytes. Splice-only slabs that never called MarkWritten pay no cost.
//
// In full-zero mode (SetFullZero(true), via -buf-zero=full), Reset
// zeroes the entire backing buffer regardless of written. That's the
// paranoid policy for deployments that don't trust callers to MarkWritten
// correctly.
func (s *Slab) Reset() {
	if fullZero.Load() {
		clear(s.b) // lowers to memclr
		s.written = 0
		return
	}
	if s.written > 0 {
		clear(s.b[:s.written])
		s.written = 0
	}
}

// fullZero toggles whole-slab zeroing. atomic.Bool because the flag is
// set once at startup but read on the hot path; atomic reads are free
// on amd64/arm64.
var fullZero atomic.Bool

// SetFullZero selects bounded (false, default) or full (true) zeroing.
// Called once from main() after flag parsing; safe to call before any
// goroutines start using slabs.
func SetFullZero(v bool) { fullZero.Store(v) }
