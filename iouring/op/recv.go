// IORING_OP_RECV — kernel-side recv(2) into a caller-supplied buffer,
// or, with IOSQE_BUFFER_SELECT set, into a provided-buffer ring slot
// the kernel picks for us (Linux 5.19+ for PBUF_RING).
//
// Multishot recv (flag IORING_RECV_MULTISHOT, Linux 6.0+) keeps firing
// CQEs as data arrives — same shape as multishot accept.
//
// Completion:
//   - cqe.Res > 0 : bytes received
//   - cqe.Res == 0 : peer closed (FIN)
//   - cqe.Res < 0  : -errno
//   - cqe.Flags & CQEFBuffer : top 16 bits of Flags hold the provided
//                              buffer ID the kernel chose.
//   - cqe.Flags & CQEFMore   : multishot still armed.

//go:build linux

package op

import "tachyon/iouring"

// Recv into `buf`. Useful when the caller owns the buffer (one-shot).
func Recv(sqe *iouring.SQE, fd int, buf []byte, userData uint64) {
	sqe.Opcode = iouring.OpRecv
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(bptr(buf)))
	sqe.Len = uint32(len(buf))
	sqe.UserData = userData
}

// RecvProvided is a one-shot recv that draws a buffer from the provided-
// buffer ring identified by bufGroup. Lets the kernel pick a buffer from
// the pool on arrival — the caller learns which via CQEFBuffer /
// iouring.BufferID.
//
// Use when you want per-arrival decision-making (e.g. stop arming once
// the request header block is parsed) but still want the shared-pool
// recv landing zone.
func RecvProvided(sqe *iouring.SQE, fd int, bufGroup uint16, userData uint64) {
	sqe.Opcode = iouring.OpRecv
	sqe.Fd = int32(fd)
	sqe.UserData = userData
	sqe.Flags = iouring.SQEBufferSelect
	sqe.BufIndex = bufGroup
}

// RecvMultishot arms a multishot recv that draws from the provided-
// buffer ring identified by bufGroup. No Addr/Len — the kernel selects
// a buffer per arrival. Callers learn which buffer via CQEFBuffer /
// iouring.BufferID.
//
// Requires Linux 6.0+ and a REGISTER_PBUF_RING group registered with
// the same bufGroup ID.
func RecvMultishot(sqe *iouring.SQE, fd int, bufGroup uint16, userData uint64) {
	sqe.Opcode = iouring.OpRecv
	sqe.Fd = int32(fd)
	sqe.UserData = userData
	sqe.Flags = iouring.SQEBufferSelect
	sqe.BufIndex = bufGroup
	// IORING_RECV_MULTISHOT goes in ioprio per the kernel ABI for recv.
	sqe.IOPrio = iouring.RecvMultishot
}
