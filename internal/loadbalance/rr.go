package loadbalance

import "sync/atomic"

// RR is atomic round-robin. Each Pick atomically increments the cursor
// and returns cursor mod n, so concurrent callers spread across the
// address list without a mutex.
type RR struct {
	cursor atomic.Uint32
}

// NewRR returns a fresh round-robin policy with an independent cursor.
func NewRR() *RR { return &RR{} }

// Pick returns the next starting index.
func (r *RR) Pick(n int) int {
	return int(r.cursor.Add(1)-1) % n
}
