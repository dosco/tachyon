// Package buffers wraps io_uring's buffer-registration features.
//
// Two mechanisms live here:
//
//   - REGISTER_BUFFERS (io_uring_register opcode 0): pins a set of user
//     buffers so SEND_ZC can reference them by index. Pointer pinning costs
//     once; every subsequent zero-copy send is just an index.
//
//   - REGISTER_PBUF_RING (opcode 22, Linux 5.19+): registers a *pool* of
//     recv buffers. When we submit RECV_MULTISHOT the kernel picks a buffer
//     from the pool per incoming datagram and returns its ID in the CQE's
//     flags. The userspace recv code becomes: ask for the next CQE, look up
//     the buffer by ID, process, return the ID.
//
// Provided buffer rings are the single most important mechanism for
// eliminating per-recv allocations. They are the reason we don't vendor an
// existing Go binding - none expose them.
package buffers
