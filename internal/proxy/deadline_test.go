package proxy

import (
	"net"
	"testing"
	"time"

	"tachyon/internal/upstream"
)

// countingConn wraps net.Pipe's side and counts SetReadDeadline /
// SetWriteDeadline calls. Used to assert the amortized cadence.
type countingConn struct {
	net.Conn
	reads, writes int
}

func (c *countingConn) SetReadDeadline(t time.Time) error {
	c.reads++
	return c.Conn.SetReadDeadline(t)
}

func (c *countingConn) SetWriteDeadline(t time.Time) error {
	c.writes++
	return c.Conn.SetWriteDeadline(t)
}

// TestClientDeadlineAmortizedCadence asserts maybeBumpClientDeadline
// fires once on first call, then elides the next 63 calls, then fires
// again on the 65th. Operator-mode "perreq" is tested separately
// below.
func TestClientDeadlineAmortizedCadence(t *testing.T) {
	a, _ := net.Pipe()
	defer a.Close()
	c := &countingConn{Conn: a}

	var uses uint32
	var at time.Time

	// First call: always bumps.
	maybeBumpClientDeadline(c, &uses, &at, false)
	if c.reads != 1 || c.writes != 1 {
		t.Fatalf("first call: reads=%d writes=%d; want 1/1", c.reads, c.writes)
	}
	if uses != 1 {
		t.Fatalf("uses after first call: got %d want 1", uses)
	}
	if at.IsZero() {
		t.Fatalf("at not set after first call")
	}

	// Calls 2..DeadlineMaxUses: must NOT bump (within refresh window).
	for i := 2; i <= DeadlineMaxUses; i++ {
		maybeBumpClientDeadline(c, &uses, &at, false)
	}
	if c.reads != 1 || c.writes != 1 {
		t.Fatalf("after 64 calls: reads=%d writes=%d; want 1/1 (amortized)", c.reads, c.writes)
	}
	if uses != DeadlineMaxUses {
		t.Fatalf("uses after 64 calls: got %d want %d", uses, DeadlineMaxUses)
	}

	// Call DeadlineMaxUses+1 must re-arm because uses hit the cap.
	maybeBumpClientDeadline(c, &uses, &at, false)
	if c.reads != 2 || c.writes != 2 {
		t.Fatalf("after cap: reads=%d writes=%d; want 2/2", c.reads, c.writes)
	}
	if uses != 1 {
		t.Fatalf("uses after cap reset: got %d want 1", uses)
	}
}

// TestClientDeadlineStrictMode asserts perreq mode bumps on every call.
func TestClientDeadlineStrictMode(t *testing.T) {
	a, _ := net.Pipe()
	defer a.Close()
	c := &countingConn{Conn: a}

	var uses uint32
	var at time.Time

	for i := 0; i < 10; i++ {
		maybeBumpClientDeadline(c, &uses, &at, true /* strict */)
	}
	if c.reads != 10 || c.writes != 10 {
		t.Fatalf("strict mode after 10 calls: reads=%d writes=%d; want 10/10", c.reads, c.writes)
	}
}

// TestClientDeadlineTimeRefresh asserts the wall-clock branch of the
// amortized policy. We stuff `at` with a timestamp older than
// DeadlineRefresh and confirm the next call bumps even though `uses`
// is still below the cap.
func TestClientDeadlineTimeRefresh(t *testing.T) {
	a, _ := net.Pipe()
	defer a.Close()
	c := &countingConn{Conn: a}

	var uses uint32 = 2
	at := time.Now().Add(-2 * DeadlineRefresh) // well past the window

	maybeBumpClientDeadline(c, &uses, &at, false)
	if c.reads != 1 || c.writes != 1 {
		t.Fatalf("time-refresh branch: reads=%d writes=%d; want 1/1", c.reads, c.writes)
	}
	if uses != 1 {
		t.Fatalf("uses after time refresh: got %d want 1", uses)
	}
}

// TestUpstreamDeadlineAmortized mirrors the client test for the
// upstream helper. Uses a real upstream.Conn wrapping a net.Pipe end.
func TestUpstreamDeadlineAmortized(t *testing.T) {
	a, _ := net.Pipe()
	defer a.Close()
	cc := &countingConn{Conn: a}
	uc := &upstream.Conn{Conn: cc}

	// First call bumps.
	maybeBumpUpstreamDeadline(uc, false)
	if cc.reads != 1 || cc.writes != 1 {
		t.Fatalf("first upstream bump: reads=%d writes=%d; want 1/1", cc.reads, cc.writes)
	}
	if uc.DeadlineUses != 1 {
		t.Fatalf("uses: got %d want 1", uc.DeadlineUses)
	}

	for i := 2; i <= DeadlineMaxUses; i++ {
		maybeBumpUpstreamDeadline(uc, false)
	}
	if cc.reads != 1 || cc.writes != 1 {
		t.Fatalf("after 64: reads=%d writes=%d; want 1/1", cc.reads, cc.writes)
	}

	maybeBumpUpstreamDeadline(uc, false)
	if cc.reads != 2 || cc.writes != 2 {
		t.Fatalf("cap triggered: reads=%d writes=%d; want 2/2", cc.reads, cc.writes)
	}
}

// TestReleaseResetsDeadlineBookkeeping asserts that when a Conn goes
// back to the pool and is re-acquired, the next upstream bump re-arms
// the deadline — otherwise the previous era's 2-min window would
// leak into the next borrower.
func TestReleaseResetsDeadlineBookkeeping(t *testing.T) {
	a, _ := net.Pipe()
	defer a.Close()
	cc := &countingConn{Conn: a}
	uc := &upstream.Conn{Conn: cc}

	maybeBumpUpstreamDeadline(uc, false)
	if uc.DeadlineAt.IsZero() || uc.DeadlineUses == 0 {
		t.Fatalf("post-bump: DeadlineAt=%v Uses=%d", uc.DeadlineAt, uc.DeadlineUses)
	}

	// Simulate what upstream.Pool.Release does.
	uc.DeadlineAt = time.Time{}
	uc.DeadlineUses = 0

	maybeBumpUpstreamDeadline(uc, false)
	if cc.reads != 2 || cc.writes != 2 {
		t.Fatalf("post-release bump: reads=%d writes=%d; want 2/2", cc.reads, cc.writes)
	}
}
