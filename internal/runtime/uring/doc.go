// Package uring is tachyon's io_uring-driven worker.
//
// # Model
//
// One Ring per worker. One goroutine per worker (GOMAXPROCS=1). No
// goroutines per connection. Every I/O is a submitted SQE; every
// completion is a CQE routed back to its owning connection via the
// user_data word.
//
// Client reads go through a single IORING_REGISTER_PBUF_RING group —
// the kernel picks a buffer from our registered set when a packet
// arrives; we copy out into the connection's accumulator buffer and
// recycle the provided buffer immediately.
//
// Upstream I/O uses plain op.Send / op.Recv on fds borrowed from a
// per-worker fd pool that pre-dials a configurable number of idle
// conns at startup. Dials don't happen on the hot path.
//
// # State machine
//
// Every connection lives in a fixed-size slab of `conn` structs,
// indexed by a 24-bit slot ID carried in the high half of user_data.
// Slot reuse is detected by a 32-bit seq counter, also packed in
// user_data, so late CQEs for a closed conn get discarded instead of
// acting on the next conn that occupies the slot.
//
// States:
//
//	stReadingRequest — recv accumulating header block from client
//	stSendingUpRequest — request bytes going out to upstream
//	stReadingUpResponse — recv accumulating header block from upstream
//	stSendingClientResponse — response bytes going out to client
//	stDraining — body pass-through in one of two directions
//	stClosing — final close op in flight
//
// # Scope of v1
//
//   - HTTP/1.1 only.
//   - No chunked request body support (rare in proxy workloads).
//   - No TLS (Phase 4).
//   - Upstream writes/reads are plain op.Send/op.Recv, not zero-copy
//     (SEND_ZC comes in Phase 3).
//   - Content-Length bodies only; chunked responses pass through as
//     opaque bytes until EOF, relying on upstream framing.
//
// None of those are structural; each slots in without rearranging the
// event loop.
package uring
