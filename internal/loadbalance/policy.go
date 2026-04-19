// Package loadbalance holds upstream selection policies. The dialer in
// internal/upstream calls Policy.Pick to choose a starting address; it
// then walks the remaining addresses sequentially on dial failure.
//
// Keeping the interface narrow (just "pick a starting index") lets
// round-robin stay zero-cost on the single-address fast path and lets
// later policies (p2c-EWMA) read per-address stats without widening the
// hot path for callers that don't use them.
package loadbalance

// Policy picks a starting address index in [0, n). n is guaranteed > 0
// by the caller. Implementations may be stateful and must be safe for
// concurrent use.
type Policy interface {
	Pick(n int) int
}
