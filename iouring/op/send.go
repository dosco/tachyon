// IORING_OP_SEND + IORING_OP_SEND_ZC.
//
// SEND is the straightforward one-copy send. SEND_ZC (Linux 6.0+) skips
// the skb copy: the kernel pins the user page and DMAs from it, then
// produces *two* CQEs per send — the usual result CQE plus a "buffer
// released" notification (CQEFNotif). The caller must not recycle the
// buffer until the notif arrives.
//
// Completion (SEND):
//   - cqe.Res > 0 : bytes sent
//   - cqe.Res < 0 : -errno
//
// Completion (SEND_ZC): same, plus a follow-up CQE with CQEFNotif and
// the same userData.

//go:build linux

package op

import "tachyon/iouring"

// Send a single buffer. Use MSG_* flags via `msgFlags` (e.g. MSG_DONTWAIT,
// MSG_NOSIGNAL). tachyon sets MSG_NOSIGNAL so a peer-closed connection
// returns EPIPE to the CQE instead of killing the process.
func Send(sqe *iouring.SQE, fd int, buf []byte, msgFlags uint32, userData uint64) {
	sqe.Opcode = iouring.OpSend
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(bptr(buf)))
	sqe.Len = uint32(len(buf))
	sqe.OpFlags = msgFlags
	sqe.UserData = userData
}

// SendZC — zero-copy send. Expect two CQEs per send; recycle the buffer
// only after the CQEFNotif arrives.
func SendZC(sqe *iouring.SQE, fd int, buf []byte, msgFlags uint32, userData uint64) {
	sqe.Opcode = iouring.OpSendZC
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(bptr(buf)))
	sqe.Len = uint32(len(buf))
	sqe.OpFlags = msgFlags
	sqe.UserData = userData
}
