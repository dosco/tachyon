// Package iouring is a minimal Linux io_uring binding for tachyon.
//
// # Status
//
// Phase 2 scaffolding. The Phase 0/1 proxy uses stdlib net; this package
// exists so the file tree matches the plan and call sites can be laid out
// in advance. The syscalls land next.
//
// # Plan
//
// tachyon does not depend on any third-party io_uring binding. None of the
// maintained candidates (Iceber/iouring-go, pawelgaczynski/gain) expose
// IORING_REGISTER_PBUF_RING (provided buffer rings) or IORING_OP_SEND_ZC,
// and both are structural to what makes tachyon fast. We write a small
// binding and own it.
//
// # Layout (planned)
//
//   - setup.go     - io_uring_setup syscall, params struct
//   - mmap.go      - SQ ring, CQ ring, SQE array mmap
//   - enter.go     - io_uring_enter (SQPOLL-aware)
//   - register.go  - IORING_REGISTER_* generic dispatcher
//   - sqe.go       - SQE struct, prep helpers, submit
//   - cqe.go       - CQE struct, batch drain, user_data routing
//   - ring.go      - Ring type composing the above
//
// # Sub-packages
//
//   - iouring/buffers - REGISTER_BUFFERS + REGISTER_PBUF_RING (the zero-copy primitive)
//   - iouring/op      - one operation per file
//
// Every op file is small and symmetric: prep the SQE, document the kernel
// version that introduced it, and name the completion semantics.
package iouring
