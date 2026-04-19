// Flow-control windows, per connection and per stream.
//
// RFC 7540 §5.2: both endpoints track a 31-bit signed window. DATA frames
// decrement the receive window; WINDOW_UPDATE replenishes it. A sender
// MUST NOT send more DATA bytes than the smaller of (conn send window,
// stream send window).
//
// We use int32 throughout. Negative values are legal (settings changes
// can drive a window below zero transiently) but we cap negative drift
// at a sanity bound and treat deeper negatives as FLOW_CONTROL_ERROR.

//go:build linux

package http2

// Window is a single-direction flow control counter.
type Window struct {
	remaining int32
}

// NewWindow seeds a window at initial.
func NewWindow(initial int32) Window { return Window{remaining: initial} }

// Take attempts to consume n bytes from the window. Reports whether the
// post-take remaining went negative (caller should send FLOW_CONTROL_ERROR).
func (w *Window) Take(n int32) (ok bool) {
	w.remaining -= n
	return w.remaining >= 0
}

// Give credits back (WINDOW_UPDATE received). Returns false on overflow
// past (2^31 - 1), which is a PROTOCOL_ERROR per RFC.
func (w *Window) Give(n int32) bool {
	if w.remaining > 0x7fffffff-n {
		return false
	}
	w.remaining += n
	return true
}

// Remaining is the current window size.
func (w *Window) Remaining() int32 { return w.remaining }

// ReceiveWindow adds auto-replenish logic: whenever the window drops
// below refill, we surface a credit to give back. The proxy converts that
// credit into a WINDOW_UPDATE frame at the next submit.
type ReceiveWindow struct {
	remaining int32
	initial   int32
	refill    int32 // low-water mark
}

// NewReceiveWindow: initial is the advertised SETTINGS_INITIAL_WINDOW_SIZE;
// refill is the low-water mark (usually initial/2).
func NewReceiveWindow(initial int32) ReceiveWindow {
	return ReceiveWindow{remaining: initial, initial: initial, refill: initial / 2}
}

// Consume records that n bytes of inbound DATA arrived. Returns the
// credit to replenish if we've crossed the low-water mark; 0 otherwise.
func (r *ReceiveWindow) Consume(n int32) (credit int32) {
	r.remaining -= n
	if r.remaining <= r.refill {
		credit = r.initial - r.remaining
		r.remaining = r.initial
	}
	return credit
}
