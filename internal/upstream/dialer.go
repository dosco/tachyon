package upstream

import (
	"net"
	"time"

	"tachyon/internal/loadbalance"
)

// dialer chooses an address and dials it. Address selection is delegated
// to a loadbalance.Policy (round-robin by default) so future policies
// (p2c-EWMA etc.) can plug in without touching the dial/retry loop. We
// do a bounded retry: each dial gets two attempts, with a short backoff
// between them, iterating over the address list on every attempt.
// Larger retry policies belong to the call site (circuit breaker on the
// pool; caller-visible 502 semantics), not here.
type dialer struct {
	addrs   []string
	timeout time.Duration

	// policy overrides the default RR. Leave nil to use rr below (the
	// common case). A custom policy (e.g. p2c-EWMA) plugs in here.
	policy loadbalance.Policy
	// rr is the default round-robin cursor. Its zero value is usable,
	// so a bare &dialer{addrs: ...} literal works without initialization.
	rr loadbalance.RR

	// outlier is the optional passive-ejection detector. Nil when not
	// configured — the walk below skips the ejection check entirely in
	// that case (single-branch cost).
	outlier *loadbalance.Detector

	// probe is the optional active health prober. Nil when not configured.
	// When set, the address walk also skips addresses that the prober
	// marks as unhealthy, with the same two-pass fallback as ejection
	// (first pass skips unhealthy, second pass tries everything so a
	// fully-down pool still attempts rather than silently erroring).
	probe *loadbalance.Prober

	// attempts bounds the retry loop. Zero means "use default".
	attempts int
	// backoff is the sleep between attempts. Zero means "use default".
	backoff time.Duration

	// dialFn is indirected for tests. Production uses net.DialTimeout.
	dialFn func(network, addr string, timeout time.Duration) (net.Conn, error)
}

// defaultDialAttempts / defaultDialBackoff are Phase 1 defaults. Tuned to
// cover a single transient upstream hiccup (TCP RST on exhausted accept
// queue, brief network blip) without multiplying the client-visible tail
// latency on a truly down backend — the pool's circuit breaker turns a hard
// failure into a fast 502 after a few dial rounds.
const (
	defaultDialAttempts = 2
	defaultDialBackoff  = 50 * time.Millisecond
)

func (d *dialer) dial() (*Conn, error) {
	n := len(d.addrs)
	if n == 0 {
		return nil, errNoAddrs
	}
	dialFn := d.dialFn
	if dialFn == nil {
		dialFn = net.DialTimeout
	}
	attempts := d.attempts
	if attempts <= 0 {
		attempts = defaultDialAttempts
	}
	backoff := d.backoff
	if backoff <= 0 {
		backoff = defaultDialBackoff
	}

	// Pick via the configured policy if one is set, otherwise the
	// embedded RR (zero-value usable, no init needed).
	var policy loadbalance.Policy = &d.rr
	if d.policy != nil {
		policy = d.policy
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		// Re-pick on every attempt so the retry starts from a different
		// address than the one that just failed.
		start := policy.Pick(n)
		// First pass respects ejection; if every addr is ejected (or
		// outlier detection is off), walk once more ignoring ejection
		// so a fully-degraded pool still attempts a dial rather than
		// silently returning the previous attempt's error.
		for _, skipEjected := range [2]bool{true, false} {
			tried := false
			for i := 0; i < n; i++ {
				idx := (start + i) % n
				if skipEjected && d.outlier != nil && d.outlier.Ejected(idx) {
					continue
				}
				if skipEjected && d.probe != nil && !d.probe.Healthy(idx) {
					continue
				}
				tried = true
				addr := d.addrs[idx]
				c, err := dialFn("tcp", addr, d.timeout)
				if err != nil {
					lastErr = err
					continue
				}
				// Disable Nagle for minimal outbound latency. Read/write
				// timeouts are applied per-op by the proxy glue, not here.
				var tcp *net.TCPConn
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetNoDelay(true)
					_ = tc.SetKeepAlive(true)
					_ = tc.SetKeepAlivePeriod(30 * time.Second)
					tcp = tc
				}
				return &Conn{Conn: c, TCP: tcp, Addr: addr, AddrIdx: idx, LastUsed: time.Now()}, nil
			}
			// If the first pass actually dialed something (i.e. not all
			// addrs were ejected), skip the second pass — the second
			// pass is only there to rescue the all-ejected case.
			if tried {
				break
			}
		}
		// All addresses failed on this attempt. Sleep briefly and retry —
		// unless this was the last attempt.
		if attempt < attempts-1 {
			time.Sleep(backoff)
		}
	}
	return nil, lastErr
}

// errNoAddrs is a sentinel so callers can distinguish config errors from
// transient dial failures.
type dialErr string

func (e dialErr) Error() string { return string(e) }

const errNoAddrs = dialErr("upstream: pool has no addresses")
