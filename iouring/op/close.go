// IORING_OP_CLOSE — async close(2).
//
// Used so teardown doesn't block the event loop. A CQE arrives with
// Res=0 on success or -errno.

//go:build linux

package op

import "tachyon/iouring"

// Close submits an async close of fd. userData tags the CQE.
func Close(sqe *iouring.SQE, fd int, userData uint64) {
	sqe.Opcode = iouring.OpClose
	sqe.Fd = int32(fd)
	sqe.UserData = userData
}
