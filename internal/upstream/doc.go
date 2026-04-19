// Package upstream manages connections to origin servers.
//
// Design goal: maximise reuse. Pingora's measured wins come from a 99.92%
// upstream reuse rate. We match that by keeping a warm pool per worker, by
// never closing a conn that responded cleanly with keep-alive, and by using
// per-flow client affinity (BPF steering, Phase 2+) so the same client
// returns to the same worker and finds the same warm conn.
//
// # Phase 0
//
// This first cut uses net.DialTimeout, a simple mutex-guarded ring per named
// upstream, and a round-robin address picker. It is the baseline that proves
// the proxy works end-to-end. The hot pool (no-lock variant for GOMAXPROCS=1
// workers) is in hotpool.go and is swapped in by the runtime package once
// we're confident each Pool is exclusively owned by one goroutine.
//
// # Layout
//
//   - pool.go     - named pools keyed by upstream name; Acquire/Release
//   - hotpool.go  - single-goroutine lock-free ring
//   - conn.go     - upstream connection wrapper
//   - dialer.go   - address selection + Dial
package upstream
