package upstream

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"tachyon/internal/router"
)

// fakeConn is a minimal net.Conn used by the pool tests. It records
// whether Close has been called so the reaper and broken-evict paths can
// be asserted without real sockets.
type fakeConn struct {
	closed atomic.Bool
}

func (f *fakeConn) Read(b []byte) (int, error)   { return 0, net.ErrClosed }
func (f *fakeConn) Write(b []byte) (int, error)  { return len(b), nil }
func (f *fakeConn) Close() error                 { f.closed.Store(true); return nil }
func (f *fakeConn) LocalAddr() net.Addr          { return fakeAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr         { return fakeAddr{} }
func (f *fakeConn) SetDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "tcp" }
func (fakeAddr) String() string  { return "fake" }

// newTestPool returns a *Pool suitable for unit tests. The reaper still
// runs in its own goroutine; individual tests stop it via p.Close when
// they care about fake time.
func newTestPool(t *testing.T, def router.Upstream) *Pool {
	t.Helper()
	p := newPool(def)
	t.Cleanup(func() { p.Close() })
	return p
}

// TestAcquireSkipsBroken covers the loop in Acquire that drops idle conns
// which were marked broken after Release.
func TestAcquireSkipsBroken(t *testing.T) {
	p := newTestPool(t, router.Upstream{Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 4})

	// Install a dialFn that would obviously fail — we want to confirm
	// Acquire takes a broken-idle off the stack before trying to dial,
	// then (after all are broken) does reach the dialer.
	dialCount := 0
	p.d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		dialCount++
		return &fakeConn{}, nil
	}

	f1, f2 := &fakeConn{}, &fakeConn{}
	good, broken := &Conn{Conn: f1, Addr: "fake"}, &Conn{Conn: f2, Addr: "fake"}
	broken.MarkBroken()

	// Order: good at bottom, broken at top — Acquire is LIFO so it should
	// see broken first, drop it, then return good. No dial.
	p.Release(good)
	// Release rejects broken conns outright, so bypass and push directly.
	p.mu.Lock()
	p.idle = append(p.idle, broken)
	p.mu.Unlock()

	c, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if c != good {
		t.Fatalf("Acquire returned wrong conn: got %p want %p", c, good)
	}
	if !f2.closed.Load() {
		t.Fatalf("broken conn was not closed on eviction")
	}
	if dialCount != 0 {
		t.Fatalf("Acquire dialed %d times; expected 0 (should have reused good conn)", dialCount)
	}
}

// TestCircuitBreakerOpenAndClose covers recordDialFail crossing the
// threshold, Acquire returning ErrBackendDown for the cooldown window,
// and a successful dial closing the breaker.
func TestCircuitBreakerOpenAndClose(t *testing.T) {
	p := newTestPool(t, router.Upstream{Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 4})

	var fail atomic.Bool
	fail.Store(true)
	p.d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		if fail.Load() {
			return nil, errors.New("connection refused")
		}
		return &fakeConn{}, nil
	}
	// Kill the retry budget so each Acquire counts as one failure.
	p.d.attempts = 1
	p.d.backoff = time.Nanosecond

	// Push the breaker over the threshold (3 consecutive dial failures).
	for i := 0; i < defaultBreakerThreshold; i++ {
		if _, err := p.Acquire(); err == nil {
			t.Fatalf("attempt %d: Acquire returned nil err with fail=true", i)
		}
	}
	// Next Acquire should hit the circuit breaker, not the dialer.
	dialsBefore := 0
	p.d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		dialsBefore++
		return nil, errors.New("should not be reached")
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrBackendDown) {
		t.Fatalf("circuit-open Acquire: got %v, want ErrBackendDown", err)
	}
	if dialsBefore != 0 {
		t.Fatalf("dialer called %d times while breaker open", dialsBefore)
	}

	// Force the breaker closed (bypass cooldown — we're asserting the
	// close path, not the timer). Restore a success dialFn.
	p.mu.Lock()
	p.breakerOpenUntil = time.Time{}
	p.mu.Unlock()
	fail.Store(false)
	p.d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		return &fakeConn{}, nil
	}
	c, err := p.Acquire()
	if err != nil {
		t.Fatalf("post-recovery Acquire: %v", err)
	}
	_ = c.Close()
	// A successful dial must reset failCount.
	p.mu.Lock()
	fc := p.failCount
	p.mu.Unlock()
	if fc != 0 {
		t.Fatalf("failCount after recovery: got %d want 0", fc)
	}
}

// TestReapOnceDropsOld covers the reapOnce path (synchronous; no timer
// needed). The deterministic reapLoop test lives in TestReaperSynctest.
func TestReapOnceDropsOld(t *testing.T) {
	p := newTestPool(t, router.Upstream{Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 4})
	p.reapMaxAge = time.Second

	old1, old2, fresh := &fakeConn{}, &fakeConn{}, &fakeConn{}
	now := time.Now()
	p.mu.Lock()
	p.idle = append(p.idle,
		&Conn{Conn: old1, LastUsed: now.Add(-5 * time.Second)},
		&Conn{Conn: fresh, LastUsed: now},
		&Conn{Conn: old2, LastUsed: now.Add(-2 * time.Second)},
	)
	p.mu.Unlock()

	p.reapOnce(now)

	if got := p.IdleLen(); got != 1 {
		t.Fatalf("IdleLen after reap: got %d want 1", got)
	}
	if !old1.closed.Load() || !old2.closed.Load() {
		t.Fatalf("old conns not closed: old1=%v old2=%v", old1.closed.Load(), old2.closed.Load())
	}
	if fresh.closed.Load() {
		t.Fatalf("fresh conn was closed (should be kept)")
	}
}

// TestReaperSynctest verifies the background reapLoop fires on its
// ticker and evicts stale conns. Uses synctest to avoid real wall-clock
// waits. Constructs via newPoolWithReaper so the reap fields are set
// BEFORE the goroutine starts — otherwise go test -race sees a race on
// reapInterval between the test goroutine's mutation and the reapLoop's
// initial read.
func TestReaperSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newPoolWithReaper(
			router.Upstream{Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 4},
			10*time.Millisecond, 50*time.Millisecond,
		)
		defer p.Close()

		// Place an idle conn that's already stale.
		fc := &fakeConn{}
		p.mu.Lock()
		p.idle = append(p.idle, &Conn{Conn: fc, LastUsed: time.Now().Add(-time.Second)})
		p.mu.Unlock()

		// Advance past two reaper ticks.
		time.Sleep(30 * time.Millisecond)
		synctest.Wait()

		if got := p.IdleLen(); got != 0 {
			t.Fatalf("IdleLen after reap: got %d want 0", got)
		}
		if !fc.closed.Load() {
			t.Fatalf("stale conn was not closed by reaper")
		}
	})
}

// TestReleaseRejectsBroken verifies the belt-and-braces: even if a
// caller forgets to call MarkBroken pre-Release, the pool still won't
// keep a broken conn.
func TestReleaseRejectsBroken(t *testing.T) {
	p := newTestPool(t, router.Upstream{Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 4})
	fc := &fakeConn{}
	c := &Conn{Conn: fc}
	c.MarkBroken()
	p.Release(c)
	if p.IdleLen() != 0 {
		t.Fatalf("broken conn made it into idle list")
	}
	if !fc.closed.Load() {
		t.Fatalf("broken conn was not closed on Release")
	}
}

// TestReleaseOverflowClosesConn verifies the maxIdle cap closes rather
// than leaks.
func TestReleaseOverflowClosesConn(t *testing.T) {
	p := newTestPool(t, router.Upstream{Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 1})
	keep := &fakeConn{}
	overflow := &fakeConn{}
	p.Release(&Conn{Conn: keep})
	p.Release(&Conn{Conn: overflow})
	if p.IdleLen() != 1 {
		t.Fatalf("IdleLen: got %d want 1", p.IdleLen())
	}
	if !overflow.closed.Load() {
		t.Fatalf("overflow conn was not closed")
	}
	if keep.closed.Load() {
		t.Fatalf("kept conn was closed")
	}
}

// TestOutlierEjectionRoutesAroundDeadAddr wires a configured outlier
// detector through the pool and confirms that after a streak of 5xx
// responses, the dialer stops picking the bad address.
func TestOutlierEjectionRoutesAroundDeadAddr(t *testing.T) {
	addrs := []string{"a:1", "b:2", "c:3"}
	p := newTestPool(t, router.Upstream{
		Addrs:       addrs,
		IdlePerHost: 4,
		OutlierDetection: &router.OutlierDetection{
			Consecutive5xx:    3,
			EjectionDuration:  time.Second,
			MaxEjectedPercent: 100,
		},
	})
	// Fake dialer: records which addr was tried, always succeeds.
	var dialed []string
	p.d.dialFn = func(_, addr string, _ time.Duration) (net.Conn, error) {
		dialed = append(dialed, addr)
		return &fakeConn{}, nil
	}

	// Simulate 3 consecutive 5xx on addr "b:2" (idx 1).
	for i := 0; i < 3; i++ {
		c, err := p.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		// Force the conn's AddrIdx to the bad one so RecordResult
		// attributes the 5xx correctly regardless of which addr the
		// dialer actually chose.
		c.AddrIdx = 1
		p.RecordResult(c, 503, false, 0)
		_ = c.Close()
	}

	// Now 20 more acquires: none should land on "b:2".
	dialed = dialed[:0]
	for i := 0; i < 20; i++ {
		c, err := p.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		_ = c.Close()
	}
	for _, addr := range dialed {
		if addr == "b:2" {
			t.Fatalf("ejected addr b:2 was still dialed: %v", dialed)
		}
	}
}

// TestOutlierEjectionExpiresAndReturns confirms the dialer re-admits
// an ejected address after EjectionDuration elapses.
func TestOutlierEjectionExpiresAndReturns(t *testing.T) {
	addrs := []string{"a:1", "b:2"}
	p := newTestPool(t, router.Upstream{
		Addrs:       addrs,
		IdlePerHost: 4,
		OutlierDetection: &router.OutlierDetection{
			Consecutive5xx:    2,
			EjectionDuration:  20 * time.Millisecond,
			MaxEjectedPercent: 100,
		},
	})
	p.d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		return &fakeConn{}, nil
	}
	for i := 0; i < 2; i++ {
		c, err := p.Acquire()
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		c.AddrIdx = 0
		p.RecordResult(c, 500, false, 0)
		_ = c.Close()
	}
	if !p.outlier.Ejected(0) {
		t.Fatal("addr 0 should be ejected")
	}
	time.Sleep(30 * time.Millisecond)
	if p.outlier.Ejected(0) {
		t.Fatal("addr 0 should have un-ejected after duration")
	}
}

// TestP2CRoutesToFasterAddr wires the p2c_ewma policy through the pool,
// feeds 100ms latency on addr 0 and 1ms on addr 1, and confirms that
// subsequent dials overwhelmingly favor addr 1.
func TestP2CRoutesToFasterAddr(t *testing.T) {
	addrs := []string{"slow:1", "fast:2"}
	p := newTestPool(t, router.Upstream{
		Addrs:       addrs,
		IdlePerHost: 0, // force Acquire to dial every time
		LBPolicy:    "p2c_ewma",
	})
	var dialed []string
	p.d.dialFn = func(_, addr string, _ time.Duration) (net.Conn, error) {
		dialed = append(dialed, addr)
		return &fakeConn{}, nil
	}

	// Prime EWMAs directly via RecordResult.
	for i := 0; i < 50; i++ {
		c, err := p.Acquire()
		if err != nil {
			t.Fatalf("prime acquire %d: %v", i, err)
		}
		// Pretend we just saw the latency of whichever addr this was.
		var lat uint64 = 1_000_000 // 1ms
		if c.AddrIdx == 0 {
			lat = 100_000_000 // 100ms
		}
		p.RecordResult(c, 200, false, lat)
		c.MarkBroken()
		p.Release(c)
	}

	// Now measure the distribution.
	dialed = dialed[:0]
	for i := 0; i < 200; i++ {
		c, err := p.Acquire()
		if err != nil {
			t.Fatalf("measure acquire %d: %v", i, err)
		}
		c.MarkBroken()
		p.Release(c)
	}
	slow, fast := 0, 0
	for _, a := range dialed {
		switch a {
		case "slow:1":
			slow++
		case "fast:2":
			fast++
		}
	}
	if fast <= slow*2 {
		t.Fatalf("p2c did not strongly prefer fast addr: fast=%d slow=%d", fast, slow)
	}
}

// TestPoolsCloseAllStopsReapers asserts that CloseAll is idempotent and
// reaps every pool's background goroutine.
// TestProbeSkipsUnhealthyAddr wires a pool with a health prober whose
// healthy bits are manually controlled, then confirms the dialer skips
// the unhealthy address and routes to the healthy one.
func TestProbeSkipsUnhealthyAddr(t *testing.T) {
	var callsA, callsB atomic.Int32

	def := router.Upstream{
		Addrs:       []string{"a:1", "b:2"},
		IdlePerHost: 2,
		HealthCheck: &router.HealthCheck{
			Interval: time.Hour, // won't fire; we control bits directly
			Timeout:  time.Second,
		},
	}
	p := newPool(def)
	defer p.Close()

	// Override the dialFn so we can track which address is dialled.
	p.d.dialFn = func(_, addr string, _ time.Duration) (net.Conn, error) {
		switch addr {
		case "a:1":
			callsA.Add(1)
		case "b:2":
			callsB.Add(1)
		}
		return &fakeConn{}, nil
	}

	// Mark addr[0] ("a:1") unhealthy, addr[1] ("b:2") healthy.
	p.probe.ForceHealthy(0, false)
	p.probe.ForceHealthy(1, true)

	for i := 0; i < 20; i++ {
		c, err := p.Acquire()
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		// Mark broken so Release closes the conn instead of idling it.
		// This forces a fresh dial on every iteration, making the
		// address-selection routing visible to the test.
		c.MarkBroken()
		p.Release(c)
	}
	if callsA.Load() != 0 {
		t.Fatalf("unhealthy addr got %d calls, want 0", callsA.Load())
	}
	if callsB.Load() != 20 {
		t.Fatalf("healthy addr got %d calls, want 20", callsB.Load())
	}
}

// TestProbeClosedWithPool confirms probe.Stop() is called when the
// pool is closed, i.e., that probe goroutines don't outlive the pool.
func TestProbeClosedWithPool(t *testing.T) {
	def := router.Upstream{
		Addrs:       []string{"a:1"},
		IdlePerHost: 2,
		HealthCheck: &router.HealthCheck{
			Interval: time.Hour,
			Timeout:  time.Second,
		},
	}
	p := newPool(def)
	// Close stops the probe; the probe goroutine must exit without leaking.
	// If Stop blocks forever, the test will time out.
	p.Close()
}

// TestAllowRetryRequiresBudgetConfig confirms that AllowRetry returns
// false when no retry_budget is configured — the feature is off by
// default and has zero runtime cost.
func TestAllowRetryRequiresBudgetConfig(t *testing.T) {
	def := router.Upstream{Addrs: []string{"a:1"}, IdlePerHost: 2}
	p := newPool(def)
	defer p.Close()
	if p.AllowRetry() {
		t.Fatal("AllowRetry should return false when budget is not configured")
	}
}

// TestAllowRetryDrainsAndReplenishes wires a pool with a retry budget
// and confirms AllowRetry drains after initial tokens are consumed and
// replenishes as RecordResult records successes.
func TestAllowRetryDrainsAndReplenishes(t *testing.T) {
	def := router.Upstream{
		Addrs:       []string{"a:1"},
		IdlePerHost: 2,
		RetryBudget: &router.RetryBudget{
			RetryPercent: 50, // 1 token per 2 successes
			MinTokens:    2,
		},
	}
	p := newPool(def)
	defer p.Close()

	// Drain the 2 initial tokens.
	if !p.AllowRetry() {
		t.Fatal("expected first retry allowed")
	}
	if !p.AllowRetry() {
		t.Fatal("expected second retry allowed")
	}
	if p.AllowRetry() {
		t.Fatal("expected budget exhausted")
	}

	// Create a dummy conn so RecordResult can record success.
	c := &Conn{AddrIdx: 0}
	// 2 successes → 1 new token (50% → 2 successes per token).
	p.RecordResult(c, 200, false, 1000)
	p.RecordResult(c, 200, false, 1000)
	if !p.AllowRetry() {
		t.Fatal("expected retry allowed after 2 successes")
	}
}

func TestPoolsCloseAllStopsReapers(t *testing.T) {
	defs := map[string]router.Upstream{
		"a": {Addrs: []string{"1.2.3.4:80"}, IdlePerHost: 2},
		"b": {Addrs: []string{"1.2.3.5:80"}, IdlePerHost: 2},
	}
	ps := NewPools(defs)
	if len(ps.Names()) != 2 {
		t.Fatalf("Names(): got %d want 2", len(ps.Names()))
	}
	ps.CloseAll()
	// Second CloseAll must be a no-op, not a panic on close-of-closed.
	ps.CloseAll()
}
