package upstream

import (
	"net"
	"time"
)

// Conn wraps a net.Conn to an origin server along with lightweight metadata
// used by the pool (addr, last-used time for idle reaping).
//
// We keep the concrete *net.TCPConn alongside the net.Conn interface value so
// callers can hand the raw TCPConn to `io.Copy` / `net.TCPConn.ReadFrom` and
// the kernel splice fast path fires. Method promotion through an interface-
// typed embedded field does NOT expose *TCPConn's ReadFrom, so that fast path
// is lost if we only keep the interface.
type Conn struct {
	net.Conn
	TCP      *net.TCPConn // same underlying fd as Conn when TCP; nil otherwise
	Addr     string
	AddrIdx  int // index into the pool's address list; used by outlier detector
	LastUsed time.Time

	// DeadlineAt records the wall-clock time of the last
	// SetReadDeadline / SetWriteDeadline pair on this conn. The proxy
	// handler re-arms the deadline on an amortized cadence — every
	// DeadlineMaxUses writes OR when DeadlineRefresh has elapsed since
	// the last bump — rather than per-request. Go's SetDeadline shows
	// up as ~7% of proxy CPU under pprof if called per-request, but a
	// conn sitting in a long keep-alive run must be re-armed or its
	// 2-minute window lapses mid-run.
	//
	// Zero value = "never bumped"; next maybeBumpUpstreamDeadline call
	// will set both deadlines.
	DeadlineAt   time.Time
	DeadlineUses uint32

	// broken is set when a read/write fails. A broken conn must not go
	// back into the pool.
	broken bool
}

// MarkBroken tells the pool not to reuse this conn.
func (c *Conn) MarkBroken() { c.broken = true }

// IsBroken reports whether the conn was marked unusable.
func (c *Conn) IsBroken() bool { return c.broken }
