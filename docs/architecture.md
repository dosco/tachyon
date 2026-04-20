# Architecture

tachyon is a Go L7 reverse proxy designed to beat Cloudflare's Pingora on
throughput and tail latency. This document is the reader's entry point; if
you only have time for one file, start here.

## Wager

We bet that Go, used carefully, has enough headroom in 2026 to match a
purpose-built Rust proxy. The main moves are:

- **Escape the Go scheduler.** Run N=cores worker processes, each with
  GOMAXPROCS=1 and CPU affinity. The kernel load-balances via SO_REUSEPORT;
  inside each process, one runnable goroutine and no work-stealing.
- **Escape the Go netpoll.** Drive I/O with io_uring. Multishot accept and
  recv; provided-buffer rings so recv is zero-allocation in userspace;
  SEND_ZC and SPLICE so response bodies don't touch user memory at all.
- **Escape the Go heap.** Zero allocations in the steady-state request
  path. Fixed-size request/response structs, buffer Spans instead of []byte
  slices, pooled slabs, and an append-only arena for the rare variable-size
  scratch that doesn't fit.

## Subsystems

```
cmd/tachyon                     - entrypoint: fork/pin/listen/serve

iouring/       (Phase 2)        - io_uring binding
  buffers/                      - REGISTER_BUFFERS, PBUF_RING
  op/                           - one file per op (accept, recv, send, ...)

buf/                            - slab classes + byte arena
http1/                          - zero-alloc HTTP/1.1 codec
http2/         (Phase 5)        - zero-alloc HTTP/2 server
  frame/                        - per-frame-type files
  hpack/                        - static + dynamic tables, Huffman
quic/          (Phase 8)        - hand-rolled QUIC (RFC 9000/9001/9002)
  packet/                       - long/short headers, header protection
  crypto/                       - initial secrets, AEAD, HP masking
  tls/                          - crypto/tls QUIC driver
  frame/                        - CRYPTO, STREAM, ACK, MAX_DATA, ...
  recovery/                     - RTT estimator, loss detection, PTO
  congestion/                   - NewReno (CUBIC/BBR TBD)
http3/         (Phase 8)        - HTTP/3 framing + QPACK (RFC 9114/9204)
tlsutil/       (Phase 4)        - crypto/tls glue, optional kTLS
metrics/                        - HDR histogram

internal/runtime                - worker/fork/reuseport/affinity
internal/upstream               - connection pool (Phase 0 mutex, Phase 2 lock-free)
internal/router                 - immutable radix tree, atomic reload
internal/proxy                  - the glue: ties http1 + router + upstream together
```

Top-level packages (iouring, buf, http1, http2, tlsutil, metrics) never
import `internal/`. They are standalone libraries you could `go get` and use
in another project. `internal/` holds the code that makes tachyon specifically
a *proxy* - the glue.

## Dependency graph

```
         cmd/tachyon
              |
     +--------+--------+---------+
     v        v        v         v
   router  upstream  proxy    runtime
     |        |        |         |
     |        +--> http1 ------->+---> iouring (Phase 2)
     |                 |
     +---> config      +---> buf
```

No cycles. `internal/proxy` is the only place that knows about more than one
subsystem at once; everything else sees either a parent (the proxy) or a
sibling (`http1` uses `buf`; `runtime` uses `proxy`).

## Phases

See the plan file for the full list; the short version:

0. Stdlib net, correctness. **Done.**
1. SO_REUSEPORT + GOMAXPROCS=1 workers, CPU pin. **Done.**
2. io_uring replaces netpoll. Multishot accept/recv, provided buffers.
3. SEND_ZC + SPLICE + two-tier pool + BPF flow steering.
4. TLS via crypto/tls (session tickets + async OCSP).
5. HTTP/2 server (the big one).
6. kTLS behind build tag.
7. PGO from real bench profile.
8. HTTP/3 and QUIC, hand-rolled on stdlib UDP. **In progress.**

## HTTP/3 and QUIC

tachyon terminates HTTP/3 on its own QUIC stack, no external library. The
choice mirrors the rest of the proxy: the hot paths are ours, so we see and
own every allocation. `quic/` is the transport — connection IDs, packet
protection, TLS 1.3 driven through `crypto/tls`'s QUIC API, per-space packet
number tracking, and RFC 9002 loss detection. `http3/` layers framing and
QPACK on top and hands each request stream to the existing `internal/proxy`
pipeline, so every intent policy works identically on H1, H2, and H3.

v1 uses stdlib `net.ListenUDP` with SO_REUSEPORT; io_uring UDP
(`recvmsg`/`sendmsg`/GSO) is a later phase and deferred until the TCP path's
numbers settle. Activation is the same as every other feature: a
`quic { listen ":8443" }` block in `intent/config.intent` causes the
compiler to emit a non-nil `QUICConfig()` and the worker opens a UDP
socket alongside its TCP listener. Absent block, no socket, no cost.

## Benchmark

See [benchmark.md](benchmark.md).
