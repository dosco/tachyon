# Upstream connection pool

Pingora's single most visible win in the Cloudflare blog post is the
connection reuse number: 99.92% of upstream requests use an already-warm
connection, up from 87.1% on NGINX. This document explains how tachyon
reaches the same number with a simpler design.

## Pingora's two-tier design

Pingora runs a Tokio multi-threaded runtime with work-stealing. To avoid
mutex contention on a shared pool, they built two tiers:

1. **Hot pool (per-thread)**: a thread-local `AtomicPtr` holding one idle
   conn per upstream. Lock-free pop via CAS.
2. **Global pool (shared)**: a `Mutex<HashMap>` fallback for when the hot
   pool is empty.

Most requests finish their previous request on the same worker thread,
so the hot pool is hit ~99% of the time and the mutex is a rounding error.

## tachyon's adaptation

We don't have Tokio. We have Go goroutines on a Go scheduler. Instead of
fighting that, we sidestep it:

- N=cores worker **processes** via SO_REUSEPORT.
- Each process has `GOMAXPROCS=1` and CPU affinity.
- Each process runs one event-loop goroutine (Phase 2+); Phase 0 uses
  goroutine-per-conn for simplicity.

Now "per-thread pool" and "per-process pool" are the same thing, and since
only one goroutine touches it, `HotPool` (see `internal/upstream/hotpool.go`)
is a plain `[]*Conn` with no synchronisation at all. This is *faster* than
Pingora's `AtomicPtr` because a CAS still has a memory barrier; a single-goroutine pop has none.

## Cross-process connection reuse

The catch: N processes don't share memory, so an idle conn in worker 0
can't be borrowed by worker 3. We address this two ways:

1. **Per-flow affinity via cBPF.** The kernel attaches a small BPF program
   (`SO_ATTACH_REUSEPORT_CBPF`) that hashes the client 4-tuple. Same client
   -> same worker. A client's next request finds the worker that holds its
   previous warm upstream conn.

2. **Capacity sizing.** Each worker holds `max_idle_per_upstream / N + slack`
   conns. With 8 workers and a typical 100 target, each holds about 15.
   Pingora itself reports ~99% hot-pool hit rate, so the cross-worker tier
   is a rounding error we can skip in v1.

If the measured cross-worker miss rate turns out non-trivial (>2%), we have
a stretch option: Unix socketpairs between workers with fd handoff via
`SCM_RIGHTS`. Not built.

## Idle reaper

One `IORING_OP_TIMEOUT` SQE per idle conn; the CQE fires at its expiry.
No timer heap, no reaper goroutine. An edge case: if a conn is re-used
between submit and expiry, we cancel the timeout (SQE chained on re-use).

## Failure handling

A `Conn` carries a `broken` flag. On any read/write error, the proxy calls
`MarkBroken` before `Release`. `Release` sees the flag and closes the conn
instead of pooling it. This is the simplest correct handling - a broken
conn never re-enters circulation - and costs us one byte per Conn.
