# Tracing one byte from NIC to NIC

This document walks a single response byte from the origin's network card
to the client's, labelling every subsystem it passes through and noting
where copies and allocations can sneak in.

Scenario: plaintext HTTP/1.1 GET, upstream sends a 64 KiB body, client
keep-alive. Phase-complete (io_uring + SEND_ZC + SPLICE) tachyon.

```
NIC  --interrupt-->  kernel softirq  --DMA-->  socket recv queue (upstream fd)
       |
       |  io_uring reports completion:
       |    CQE(fd=upstream, bufID=7, len=1460, flags=F_MORE)
       v
tachyon worker's CQE drain loop (internal/runtime/worker.go)
       |
       |  bufID -> &providedBuf[7]    (iouring/buffers)
       v
internal/proxy.h1_handler -- response streaming branch
       |
       |  Status + headers: parsed in place with http1.ParseResponse
       |    (buf/arena, no allocation).
       |
       |  Body: we don't read it in userspace at all.
       |    Submit IORING_OP_SPLICE upstream_fd -> pipe -> client_fd.
       v
kernel splice: pages move from upstream socket to client socket without
ever being mapped into our address space. Zero user-space bytes for the
body. One CQE when done.
       |
       v
NIC tx queue  --DMA-->  client's wire
```

The request path is the mirror image: client NIC -> softirq -> recv queue;
CQE (provided buffer) -> parse in place (`http1.Parse`) -> rewritten header
block is *the only bytes we copy* (small, fixed, to add X-Forwarded-For) ->
SEND from our wrBuf to upstream fd -> SPLICE client_fd -> pipe ->
upstream_fd for the request body.

## Where allocations could creep in

Every line in this trace has a no-alloc guarantee. The allocation audit:

- `http1.Parse` writes into fixed-size fields on a pooled `*http1.Request`.
  Headers are Spans into the recv buffer, which is a provided buffer, which
  is owned by the kernel-registered pool. **No heap.**
- The write buffer is a slab from `buf.Pool[ClassHeader]`. **No heap.**
- Upstream conn is drawn from `internal/upstream.HotPool` - a slice inside
  a per-worker struct. **No heap.**
- Router match is an atomic pointer load and a radix walk over immutable
  nodes built at config time. **No heap.**
- `splice` and `SEND_ZC` never touch our address space.

## Where copies remain (and why)

Two unavoidable copies in the request path:

1. **Header rewrite.** We must add/modify X-Forwarded-For and rewrite Host.
   That's a few dozen bytes copied from recv buffer to write buffer.
2. **Response status/headers.** Same shape, same cost.

Both are small and fixed relative to body size. For a 64 KiB body the
user-space copy budget is ~500 bytes of headers - under 1% of bytes moved.

## Verifying the claims

```
go test -run none -bench=. -benchmem ./...
```

should show 0 `allocs/op` on hot-path benchmarks. Also:

```
perf stat -e syscalls:sys_enter_* ./tachyon ... &
wrk2 -t4 -c256 -R50000 http://localhost:8080/
```

should show syscalls per request trending toward 0.1-0.3 with SQPOLL on.
