package upstream

import (
	"errors"
	"sync"
	"time"

	"tachyon/internal/loadbalance"
	"tachyon/internal/router"
)

// Pools is a set of per-upstream-name connection pools built from config.
// The proxy holds one Pools instance per worker process.
type Pools struct {
	byName map[string]*Pool
}

// NewPools constructs Pools from the parsed config's upstream map.
func NewPools(defs map[string]router.Upstream) *Pools {
	p := &Pools{byName: make(map[string]*Pool, len(defs))}
	for name, def := range defs {
		p.byName[name] = newPool(def)
	}
	return p
}

// Get returns the Pool for a named upstream, or nil.
func (p *Pools) Get(name string) *Pool { return p.byName[name] }

// Names returns the configured upstream names. Stable ordering is not
// promised; callers that need determinism should sort.
func (p *Pools) Names() []string {
	out := make([]string, 0, len(p.byName))
	for n := range p.byName {
		out = append(out, n)
	}
	return out
}

// CloseAll stops every pool's background reaper and drops every idle conn.
// Called once on shutdown (G) and on SIGHUP reload (H) for pools that are
// being replaced.
func (p *Pools) CloseAll() {
	for _, pool := range p.byName {
		pool.Close()
	}
}

// ------------------------------------------------------------------
// Pool - a single named upstream.
// ------------------------------------------------------------------

// ErrNoUpstream is returned when the router names an upstream that isn't
// configured. That's a config bug; we surface it as 502 to the client.
var ErrNoUpstream = errors.New("upstream: no such upstream")

// ErrBackendDown is returned by Acquire while the circuit breaker is open.
// Callers surface this as a fast 502 so a down backend doesn't exhaust
// client socket budgets by waiting on dial timeouts.
var ErrBackendDown = errors.New("upstream: backend circuit open")

// Pool is one origin pool: a dialer, a bounded idle list, a circuit
// breaker, and a background idle-reaper goroutine.
//
// In Phase 0 the idle list was protected by a mutex. The mutex is still
// here in Phase 1 because the idle reaper runs on its own goroutine and
// would otherwise race; on the hot path the worker process is
// GOMAXPROCS=1 so contention is essentially nil.
type Pool struct {
	d       *dialer
	maxIdle int

	mu   sync.Mutex
	idle []*Conn // LIFO: newest conns at the end; they're most likely alive

	// Circuit breaker state. breakerOpenUntil holds the monotonic wall-clock
	// time (time.Now) at which the breaker re-closes; while set in the
	// future, Acquire returns ErrBackendDown without attempting a dial.
	// failCount counts consecutive dial failures since the last success.
	breakerOpenUntil time.Time
	failCount        uint32

	// outlier is the passive-ejection detector for per-response feedback.
	// nil when outlier_detection: is omitted from the upstream config;
	// RecordResult is a fast-return no-op in that case.
	outlier *loadbalance.Detector

	// probe is the active health prober. Nil when health_check: is
	// absent; Start/Stop are no-ops in that case and the dialer skips
	// the healthy-bit check.
	probe *loadbalance.Prober

	// budget is the optional retry token bucket. Nil when retry_budget:
	// is absent; AllowRetry always returns false in that case.
	budget *loadbalance.Budget

	// stats is the optional latency-consuming policy (e.g. *P2C). Nil
	// for round-robin — RecordResult skips the latency-update branch
	// entirely in that case. We cache the interface at construction so
	// the per-request RecordResult path doesn't do a type assertion.
	stats statsUpdater

	// Reaper.
	reapInterval time.Duration
	reapMaxAge   time.Duration
	stopReap     chan struct{}
	reapDone     chan struct{}
}

// statsUpdater is a narrow internal interface implemented by policies
// that consume per-response latency (currently just P2C). Kept private
// so the pool's contract with the loadbalance package stays explicit.
type statsUpdater interface {
	Update(idx int, latencyNs uint64)
}

// Breaker tuning. A Phase 1 default; a future config knob can expose these.
const (
	defaultBreakerThreshold  = 3                // consecutive dial failures
	defaultBreakerCooldown   = 30 * time.Second // how long the breaker stays open
	defaultReaperInterval    = 30 * time.Second
	defaultReaperIdleMaxAge  = 90 * time.Second
)

func newPool(def router.Upstream) *Pool {
	return newPoolWithReaper(def, defaultReaperInterval, defaultReaperIdleMaxAge)
}

// newPoolWithReaper is the internal constructor used by newPool and by
// tests that want a tighter reaper cadence. It sets the reaper fields
// BEFORE starting the goroutine so a later mutation isn't a race
// (go test -race catches this otherwise).
func newPoolWithReaper(def router.Upstream, interval, maxAge time.Duration) *Pool {
	p := &Pool{
		d:       &dialer{addrs: def.Addrs, timeout: def.ConnectTimeout},
		maxIdle: def.IdlePerHost,
		idle:    make([]*Conn, 0, def.IdlePerHost),

		reapInterval: interval,
		reapMaxAge:   maxAge,
		stopReap:     make(chan struct{}),
		reapDone:     make(chan struct{}),
	}
	if def.OutlierDetection != nil && len(def.Addrs) > 0 {
		p.outlier = loadbalance.NewDetector(len(def.Addrs), loadbalance.OutlierConfig{
			Consecutive5xx:        def.OutlierDetection.Consecutive5xx,
			ConsecutiveGatewayErr: def.OutlierDetection.ConsecutiveGatewayErr,
			EjectionDuration:      def.OutlierDetection.EjectionDuration,
			MaxEjectedPercent:     def.OutlierDetection.MaxEjectedPercent,
		})
		p.d.outlier = p.outlier
	}
	if def.LBPolicy == "p2c_ewma" && len(def.Addrs) > 1 {
		// Single-address pools gain nothing from p2c, so we skip
		// constructing it there and keep the bench hot path unchanged.
		pc := loadbalance.NewP2C(len(def.Addrs))
		p.d.policy = pc
		p.stats = pc
	}
	if def.RetryBudget != nil {
		p.budget = loadbalance.NewBudget(loadbalance.BudgetConfig{
			RetryPercent: def.RetryBudget.RetryPercent,
			MinTokens:    def.RetryBudget.MinTokens,
		})
	}
	if def.HealthCheck != nil && len(def.Addrs) > 0 {
		pr := loadbalance.NewProber(def.Addrs, loadbalance.ProbeConfig{
			Interval: def.HealthCheck.Interval,
			Path:     def.HealthCheck.Path,
			Timeout:  def.HealthCheck.Timeout,
		})
		p.probe = pr
		p.d.probe = pr
		pr.Start()
	}
	go p.reapLoop()
	return p
}

// Acquire returns a warm conn if one exists, otherwise dials.
//
// The loop skips idle conns that are already marked broken (e.g. by a
// prior handler that observed an IO error but returned the conn via
// Release before noticing). A well-behaved handler calls MarkBroken +
// Release itself, but belt-and-suspenders here costs nothing on the hot
// path (the idle list is tiny) and prevents a class of "one stale conn
// at the tail of idle poisons every new borrower" bugs.
//
// The circuit breaker short-circuits the dial when a run of recent
// dial failures suggests the backend is down. That turns a
// connect-timeout storm (each client paying 1 s on a hung TCP RST) into
// an instant 502 for the cooldown window.
func (p *Pool) Acquire() (*Conn, error) {
	p.mu.Lock()
	// Reuse any warm idle conn. Loop skips ones flagged broken; those
	// could only end up here if something put them back post-marking,
	// but we defend anyway.
	for {
		n := len(p.idle)
		if n == 0 {
			break
		}
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		if c.IsBroken() {
			_ = c.Close()
			continue
		}
		p.mu.Unlock()
		return c, nil
	}
	// No idle conn. Respect the circuit breaker.
	if !p.breakerOpenUntil.IsZero() && time.Now().Before(p.breakerOpenUntil) {
		p.mu.Unlock()
		return nil, ErrBackendDown
	}
	p.mu.Unlock()

	c, err := p.d.dial()
	if err != nil {
		p.recordDialFail()
		return nil, err
	}
	p.recordDialOK()
	return c, nil
}

// RecordResult feeds a completed upstream request into the outlier
// detector and (if configured) the latency-stats policy. status is the
// HTTP status observed (0 when gatewayErr is true — the response never
// completed). gatewayErr is true for any post-dial IO failure (broken
// pipe, read timeout, connection reset). latencyNs is the observed
// request-to-response time in nanoseconds; callers pass 0 to skip the
// EWMA update (e.g. on gateway-error paths where latency is
// meaningless).
//
// Safe to call when outlier detection and stats are both disabled;
// becomes two predicted-not-taken branches.
func (p *Pool) RecordResult(c *Conn, status int, gatewayErr bool, latencyNs uint64) {
	if c == nil {
		return
	}
	if p.outlier != nil {
		p.outlier.Record(c.AddrIdx, status, gatewayErr)
	}
	if p.stats != nil && !gatewayErr && latencyNs > 0 {
		p.stats.Update(c.AddrIdx, latencyNs)
	}
	// A 2xx/3xx/4xx response counts as a success for budget purposes;
	// only gateway errors and 5xx do not replenish tokens.
	if p.budget != nil && !gatewayErr && status > 0 && status < 500 {
		p.budget.RecordSuccess()
	}
}

// AllowRetry returns true and consumes one retry token if the pool's
// retry budget is configured and has tokens available. Returns false
// when the budget is exhausted or not configured, preventing retry
// storms during sustained upstream failures.
func (p *Pool) AllowRetry() bool {
	if p.budget == nil {
		return false
	}
	return p.budget.AllowRetry()
}

// recordDialFail bumps the consecutive-failure counter and opens the
// breaker once the threshold is crossed.
func (p *Pool) recordDialFail() {
	p.mu.Lock()
	p.failCount++
	if p.failCount >= defaultBreakerThreshold {
		p.breakerOpenUntil = time.Now().Add(defaultBreakerCooldown)
	}
	p.mu.Unlock()
}

// recordDialOK clears the failure streak and re-closes the breaker.
func (p *Pool) recordDialOK() {
	p.mu.Lock()
	p.failCount = 0
	p.breakerOpenUntil = time.Time{}
	p.mu.Unlock()
}

// Release returns c to the idle pool if it is reusable and the pool has
// capacity. Otherwise the conn is closed. Callers must not touch c after
// Release.
func (p *Pool) Release(c *Conn) {
	if c == nil {
		return
	}
	if c.IsBroken() {
		_ = c.Close()
		return
	}
	c.LastUsed = time.Now()
	// Clear deadline bookkeeping so the next borrower re-arms. The
	// underlying kernel deadline remains until it's overwritten; when
	// the next borrower sends its first write, maybeBumpUpstreamDeadline
	// will install a fresh 2-minute window.
	c.DeadlineAt = time.Time{}
	c.DeadlineUses = 0

	p.mu.Lock()
	if len(p.idle) >= p.maxIdle {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
}

// Addr returns the first configured address (or "" if none). Used by the
// H2 adapter, which builds a net/http URL and needs a target string.
func (p *Pool) Addr() string {
	if len(p.d.addrs) == 0 {
		return ""
	}
	return p.d.addrs[0]
}

// CloseIdle closes and drops every idle conn. Used on config reload / drain.
func (p *Pool) CloseIdle() {
	p.mu.Lock()
	idle := p.idle
	p.idle = p.idle[:0]
	p.mu.Unlock()
	for _, c := range idle {
		_ = c.Close()
	}
}

// Close stops the reaper, the probe goroutine (if any), and drops
// every idle conn. Idempotent.
func (p *Pool) Close() {
	// stopReap may already be closed if Close was called twice; guard.
	select {
	case <-p.stopReap:
		// already stopped
	default:
		close(p.stopReap)
		<-p.reapDone
	}
	if p.probe != nil {
		p.probe.Stop()
	}
	p.CloseIdle()
}

// reapLoop periodically closes conns that have sat idle longer than
// reapMaxAge. Stops when stopReap is closed.
//
// Rationale: idle TCP conns are fine in principle but the server-side
// (upstream) may close a long-lived idle conn at its own idle timeout —
// often 60 or 75 seconds. Reusing such a conn gives us a write failure
// on the next Acquire and costs a request. Proactively closing conns
// older than 90 s keeps the idle pool hot and avoids that race.
func (p *Pool) reapLoop() {
	defer close(p.reapDone)
	t := time.NewTicker(p.reapInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stopReap:
			return
		case <-t.C:
			p.reapOnce(time.Now())
		}
	}
}

// reapOnce closes every idle conn whose LastUsed is older than reapMaxAge
// relative to `now`. Extracted for unit testing.
func (p *Pool) reapOnce(now time.Time) {
	p.mu.Lock()
	cutoff := now.Add(-p.reapMaxAge)
	kept := p.idle[:0]
	var dropped []*Conn
	for _, c := range p.idle {
		if c.LastUsed.Before(cutoff) {
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	p.idle = kept
	p.mu.Unlock()
	for _, c := range dropped {
		_ = c.Close()
	}
}

// IdleLen returns the current idle-pool depth. Test-only.
func (p *Pool) IdleLen() int {
	p.mu.Lock()
	n := len(p.idle)
	p.mu.Unlock()
	return n
}
