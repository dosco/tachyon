package loadbalance

import (
	"math/rand/v2"
	"sync/atomic"
)

// P2C implements power-of-two-choices with a per-address EWMA of
// response latency.
//
// On each Pick we sample two random addresses (with replacement) and
// return the one with the lower current EWMA. That's a well-known
// approximation to "always route to the least-loaded" that keeps the
// decision O(1) and avoids the herd behaviour of truly greedy
// selection. EWMA weights recent observations heavily so the policy
// reacts to transient tail-latency spikes without being whipsawed by a
// single outlier.
//
// Per-worker state: GOMAXPROCS=1 per worker means we could use plain
// uint64 fields, but we store EWMAs via atomics so that future multi-
// goroutine paths (e.g. the health-probe goroutine updating latency
// from active probes in Step 5) don't race. The atomic cost is ~1ns
// per op and isn't measurable against dial time.
type P2C struct {
	// ewmaNs is exponentially-weighted moving average of response
	// latency in nanoseconds. Zero = "no samples yet"; treated as
	// infinitely fast (prefer unknown over slow) so new addresses
	// get their first dispatch quickly and then self-correct.
	ewmaNs []atomic.Uint64
}

// alphaNum / alphaDen implement EWMA with α = 1/8: new = 7/8*old + 1/8*sample.
// Integer ratios avoid float math on the response path. 1/8 is a
// common baseline that gives ~8-sample smoothing — responsive enough
// to catch a backend going slow, smooth enough to ignore a single
// 99th-percentile sample.
const (
	alphaNum = 1
	alphaDen = 8
)

// NewP2C returns a P2C policy sized for n addresses.
func NewP2C(n int) *P2C {
	return &P2C{ewmaNs: make([]atomic.Uint64, n)}
}

// Pick returns the index with the lower EWMA of two random samples.
// If n < 2, returns 0 (the only option).
func (p *P2C) Pick(n int) int {
	if n <= 1 {
		return 0
	}
	if n != len(p.ewmaNs) {
		// Mismatched stats array — the pool rebuilt addresses without
		// rebuilding the policy. Degrade to a single random pick rather
		// than panic on a slice bounds check.
		return rand.IntN(n)
	}
	a := rand.IntN(n)
	b := rand.IntN(n - 1)
	if b >= a {
		b++
	}
	ewA := p.ewmaNs[a].Load()
	ewB := p.ewmaNs[b].Load()
	// Zero EWMA means "never measured" — prefer it (fast path for new
	// addresses and warm-start). If both are zero, either is fine.
	if ewA == 0 {
		return a
	}
	if ewB == 0 {
		return b
	}
	if ewA <= ewB {
		return a
	}
	return b
}

// Update feeds a new latency sample for address idx into the EWMA.
// Called by the pool after each completed response. latencyNs == 0 is
// treated as "no update" so the handler can skip timing on error paths
// without corrupting the stat.
func (p *P2C) Update(idx int, latencyNs uint64) {
	if idx < 0 || idx >= len(p.ewmaNs) || latencyNs == 0 {
		return
	}
	// Classic EWMA: new = (den-num)/den * old + num/den * sample.
	// Done in integer space for determinism and no float allocation.
	for {
		old := p.ewmaNs[idx].Load()
		var next uint64
		if old == 0 {
			next = latencyNs
		} else {
			next = (old*(alphaDen-alphaNum) + latencyNs*alphaNum) / alphaDen
		}
		if p.ewmaNs[idx].CompareAndSwap(old, next) {
			return
		}
	}
}
