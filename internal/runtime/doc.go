// Package runtime holds the worker process model.
//
// tachyon runs N=cores worker processes. Each has GOMAXPROCS=1 and binds a
// CPU affinity mask so the Go scheduler never migrates the one runnable
// goroutine off its core. The kernel load-balances incoming connections via
// SO_REUSEPORT, optionally steered by an attached cBPF program that hashes
// the client 4-tuple for per-flow affinity.
//
// On macOS/dev builds the worker runs single-process with GOMAXPROCS default
// - fork/exec and SO_REUSEPORT tricks are Linux-specific. The code is
// arranged so the proxy logic (internal/proxy) is identical either way.
//
// # Layout
//
//   - worker.go    - Serve loop; accepts connections and dispatches to proxy
//   - fork.go      - fork/exec N workers (Linux), or inline (dev)
//   - reuseport.go - listen with SO_REUSEPORT + optional BPF steering
//   - affinity.go  - sched_setaffinity + LockOSThread (Linux)
package runtime
