// IORING_OP_SPLICE — move bytes between two fds without copying to user
// space. A per-worker pipe pair bridges socket→socket: source fd →
// pipe[1], pipe[0] → destination fd. No bytes ever touch userland.

//go:build linux

package op

import "tachyon/iouring"

// Splice moves up to nbytes bytes from fdIn (at offIn) to fdOut (at
// offOut). Pass -1 for offsets that don't apply (sockets). spliceFlags
// are Linux SPLICE_F_* bits (MOVE, NONBLOCK, MORE, GIFT).
func Splice(sqe *iouring.SQE, fdIn int, offIn int64, fdOut int, offOut int64, nbytes uint32, spliceFlags uint32, userData uint64) {
	sqe.Opcode = iouring.OpSplice
	sqe.Fd = int32(fdOut)
	sqe.Len = nbytes
	sqe.Off = uint64(offOut)
	// The kernel packs splice's second fd + in-offset into SpliceFdIn + Addr.
	sqe.SpliceFdIn = int32(fdIn)
	sqe.Addr = uint64(offIn)
	sqe.OpFlags = spliceFlags
	sqe.UserData = userData
}
