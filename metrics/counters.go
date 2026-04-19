package metrics

import "sync/atomic"

// Counters is the hot-path counter set. One instance per worker; reads
// are cheap (atomic.Load) for the /metrics endpoint; writes are cheap
// (atomic.Add) on the request path — 1-2 ns on amd64/arm64.
//
// We track only what's useful for production observability without a
// tag explosion. Per-route or per-upstream breakdowns are out of scope;
// operators who need those can layer log-based aggregation on top.
type Counters struct {
	// Requests is total requests accepted and dispatched by the worker.
	// Incremented once per request at the top of the keep-alive loop,
	// BEFORE routing. Counts 404s and malformed requests as "received".
	Requests atomic.Uint64

	// OK2xx counts responses with status 200-299 returned to the client.
	OK2xx atomic.Uint64

	// Err4xx counts 400-499 returned to the client — client-fault
	// responses (bad request, route not found, expect=100 rejected).
	Err4xx atomic.Uint64

	// Err5xx counts 500-599 returned to the client — gateway failures
	// (upstream dial, upstream read, etc).
	Err5xx atomic.Uint64

	// UpDialErr counts upstream Acquire failures (dial retries
	// exhausted, circuit breaker open). These become 502s to the
	// client, so they also increment Err5xx.
	UpDialErr atomic.Uint64

	// UpWriteErr counts upstream Write failures after a successful
	// Acquire. Also becomes a 502 to the client.
	UpWriteErr atomic.Uint64

	// UpReadErr counts upstream Read failures: short header reads,
	// malformed responses, EOF before framing completed. Also 502.
	UpReadErr atomic.Uint64
}

// Global is the process-wide Counters singleton. The handler package
// references this; the /metrics endpoint reads it. Using a singleton
// (instead of threading a *Counters through every handler call) is a
// deliberate choice: (1) there's only ever one metric space per worker
// process, (2) atomics on a singleton are zero-overhead on the hot
// path — no pointer dereference beyond the address of the field, (3)
// test setups don't need to construct or inject it.
var Global Counters

// RecordStatus bumps the appropriate status-class counter based on code.
// Cheap; two atomic adds in the worst case (total + class).
func RecordStatus(code int) {
	Global.Requests.Add(1)
	switch {
	case code >= 200 && code < 300:
		Global.OK2xx.Add(1)
	case code >= 400 && code < 500:
		Global.Err4xx.Add(1)
	case code >= 500 && code < 600:
		Global.Err5xx.Add(1)
	}
}

// Snapshot reads the counters into a plain struct for the /metrics
// response. Atomic reads; no blocking. Not exactly synchronized across
// counters (we don't take a lock), but for Prometheus scrape purposes
// a tiny temporal skew is irrelevant.
type Snapshot struct {
	Requests   uint64
	OK2xx      uint64
	Err4xx     uint64
	Err5xx     uint64
	UpDialErr  uint64
	UpWriteErr uint64
	UpReadErr  uint64
}

// Read loads a consistent snapshot of the counters. "Consistent" in
// the sense of atomic reads each; not a global snapshot.
func Read() Snapshot {
	return Snapshot{
		Requests:   Global.Requests.Load(),
		OK2xx:      Global.OK2xx.Load(),
		Err4xx:     Global.Err4xx.Load(),
		Err5xx:     Global.Err5xx.Load(),
		UpDialErr:  Global.UpDialErr.Load(),
		UpWriteErr: Global.UpWriteErr.Load(),
		UpReadErr:  Global.UpReadErr.Load(),
	}
}
