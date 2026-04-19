package loadbalance

import (
	"sync"
	"time"
)

// OutlierConfig controls passive ejection. Zero-value fields fall back
// to defaults in NewDetector; a nil *OutlierConfig at the pool means
// outlier detection is disabled entirely (no allocation, no per-response
// callback cost — the handler's RecordResult becomes a no-op).
type OutlierConfig struct {
	// Consecutive5xx ejects an address after this many back-to-back
	// 5xx responses. 0 → default 5.
	Consecutive5xx int
	// ConsecutiveGatewayErr ejects an address after this many back-to-
	// back gateway errors (connect refused, read/write failures after
	// dial). 0 → default 5.
	ConsecutiveGatewayErr int
	// EjectionDuration is how long an ejected address stays out of the
	// rotation. 0 → default 30s.
	EjectionDuration time.Duration
	// MaxEjectedPercent caps the fraction of addresses that may be
	// ejected at once. If ejecting one more would cross the cap, the
	// ejection is skipped (better to route to a possibly-degraded host
	// than to starve the pool). 0 → default 50.
	MaxEjectedPercent int
}

const (
	defaultConsecutive5xx        = 5
	defaultConsecutiveGatewayErr = 5
	defaultEjectionDuration      = 30 * time.Second
	defaultMaxEjectedPercent     = 50
)

// Detector tracks per-address error streaks and ejection windows. One
// Detector per Pool, shared across worker goroutines via mu.
//
// GOMAXPROCS=1 per worker means mu is usually uncontended; the cost of
// the lock on the response path is a predictable ~10ns. We pick a lock
// over atomics because the ejection decision reads and writes three
// fields consistently and a spurious ejection from a torn read would
// be observable.
type Detector struct {
	cfg OutlierConfig
	n   int

	mu           sync.Mutex
	consec5xx    []int
	consecGw     []int
	ejectedUntil []time.Time
	numEjected   int
}

// NewDetector builds a Detector for n addresses. Callers should pass a
// zero-valued OutlierConfig to use defaults; zero fields within a
// mostly-configured struct also fall back to defaults individually.
func NewDetector(n int, cfg OutlierConfig) *Detector {
	if cfg.Consecutive5xx <= 0 {
		cfg.Consecutive5xx = defaultConsecutive5xx
	}
	if cfg.ConsecutiveGatewayErr <= 0 {
		cfg.ConsecutiveGatewayErr = defaultConsecutiveGatewayErr
	}
	if cfg.EjectionDuration <= 0 {
		cfg.EjectionDuration = defaultEjectionDuration
	}
	if cfg.MaxEjectedPercent <= 0 {
		cfg.MaxEjectedPercent = defaultMaxEjectedPercent
	}
	return &Detector{
		cfg:          cfg,
		n:            n,
		consec5xx:    make([]int, n),
		consecGw:     make([]int, n),
		ejectedUntil: make([]time.Time, n),
	}
}

// Record updates counters for address idx. status is the upstream HTTP
// status (0 when gatewayErr is true — the response never completed).
// gatewayErr is true for any post-dial IO failure (broken pipe, read
// timeout, connection reset). Dial-time errors are handled by the
// pool's existing circuit breaker and don't flow here.
func (d *Detector) Record(idx int, status int, gatewayErr bool) {
	if d == nil || idx < 0 || idx >= d.n {
		return
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	switch {
	case gatewayErr:
		d.consecGw[idx]++
		d.consec5xx[idx] = 0
		if d.consecGw[idx] >= d.cfg.ConsecutiveGatewayErr {
			d.tryEject(idx, now)
		}
	case status >= 500 && status < 600:
		d.consec5xx[idx]++
		d.consecGw[idx] = 0
		if d.consec5xx[idx] >= d.cfg.Consecutive5xx {
			d.tryEject(idx, now)
		}
	default:
		// 2xx/3xx/4xx — a valid response; clear both streaks.
		d.consec5xx[idx] = 0
		d.consecGw[idx] = 0
	}
}

// tryEject ejects idx if the max-ejected cap allows it. Caller holds mu.
func (d *Detector) tryEject(idx int, now time.Time) {
	if !d.ejectedUntil[idx].IsZero() && now.Before(d.ejectedUntil[idx]) {
		return // already ejected
	}
	// Refresh count: any expired ejections free up a slot.
	live := 0
	for i, t := range d.ejectedUntil {
		if i == idx {
			continue
		}
		if !t.IsZero() && now.Before(t) {
			live++
		}
	}
	// Would ejecting idx exceed the cap?
	cap := (d.n * d.cfg.MaxEjectedPercent) / 100
	if live+1 > cap {
		// Skip ejection, but reset the streak so we don't re-trigger
		// on the very next 5xx.
		d.consec5xx[idx] = 0
		d.consecGw[idx] = 0
		return
	}
	d.ejectedUntil[idx] = now.Add(d.cfg.EjectionDuration)
	d.consec5xx[idx] = 0
	d.consecGw[idx] = 0
	d.numEjected = live + 1
}

// Ejected reports whether idx is currently out of rotation. Called on
// the dial hot path; cheap (one lock, one compare, one time.Before).
func (d *Detector) Ejected(idx int) bool {
	if d == nil || idx < 0 || idx >= d.n {
		return false
	}
	d.mu.Lock()
	t := d.ejectedUntil[idx]
	d.mu.Unlock()
	if t.IsZero() {
		return false
	}
	return time.Now().Before(t)
}
