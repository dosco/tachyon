// Per-stream state. One Stream per active (conn, stream_id).
//
// HTTP/2 stream lifecycle (server-side, elided for unused transitions):
//
//	idle  --HEADERS-->   open
//	open  --ES(recv)-->  half-closed (remote)
//	open  --ES(send)-->  half-closed (local)
//	any   --RST_STREAM-> closed
//	any   --END_STREAM * 2--> closed
//
// We do not use this enum for transition validation — the conn drives
// state changes directly and rejects frames that violate the state by
// emitting RST_STREAM with STREAM_CLOSED. The State field is kept for
// diagnostics and so Close() is idempotent.

//go:build linux

package http2

// State is the stream state per RFC 7540 §5.1. idle / reserved-* states
// are not reachable here because we're server-only and don't push.
type State uint8

const (
	StateIdle State = iota
	StateOpen
	StateHalfClosedRemote
	StateHalfClosedLocal
	StateClosed
)

// Stream holds per-stream accounting. No heap: the parent Conn owns a
// fixed `[maxStreams]Stream` slab and allocates slots out of a freelist.
type Stream struct {
	ID    uint32
	State State

	// Flow windows.
	SendWindow int32         // bytes we may still send (peer-controlled)
	RecvWindow ReceiveWindow // bytes peer may still send (we control)

	// Request shape. Populated by HEADERS/CONTINUATION decoding.
	method, path, authority, scheme []byte // slices into the header decode arena

	// Body accounting (if any).
	ContentLength int64 // -1 = unknown
	EndStreamSeen bool
}

// Reset zeroes the stream in-place for reuse without freeing memory.
func (s *Stream) Reset() {
	*s = Stream{}
}

// IsClosed reports whether any further frames on this stream are illegal
// (beyond in-flight window updates / RST_STREAMs).
func (s *Stream) IsClosed() bool { return s.State == StateClosed }
