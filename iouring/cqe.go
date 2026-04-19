// CQE drain.
//
// The kernel produces CQEs into a mmap'd ring. The tail index moves
// under our feet; head we advance to "consume" an entry. A CQE carries:
//
//   - user_data: opaque 64-bit we set when prepping the SQE. tachyon
//     uses it to tag (op-type, connection-slot) so the dispatcher can
//     route completions without a map lookup.
//   - res:       the syscall return (negative errno, bytes transferred,
//                accepted fd, etc).
//   - flags:     kernel hint bits. IORING_CQE_F_MORE for multishot,
//                IORING_CQE_F_BUFFER to indicate a buffer ID in the top
//                16 bits, IORING_CQE_F_NOTIF for send_zc notifications.

//go:build linux

package iouring

import (
	"sync/atomic"
)

// BufferID extracts the provided-buffer group ID from a CQE's flags
// word. Only meaningful when (cqe.Flags & CQEFBuffer) != 0.
//
// Kernel layout: the top 16 bits of the flags word hold the buffer ID.
func BufferID(c *CQE) uint16 {
	return uint16(c.Flags >> 16)
}

// Drain is a range-over-func iterator that yields every ready CQE. It
// advances the kernel-visible head as each entry is yielded — if the
// caller returns false, only the entries actually visited are released.
//
// Zero-alloc; the CQE pointer is re-used between iterations and stays
// valid only until the next yield.
func (r *Ring) Drain(yield func(*CQE) bool) {
	for {
		tail := atomic.LoadUint32(r.m.cqTail) // acquire: kernel writes it
		head := *r.m.cqHead                    // we own head
		if head == tail {
			return
		}
		cqe := r.m.cqeAt(head & *r.m.cqMask)
		// Advance head *before* yielding so the kernel can refill the
		// slot if it's waiting. Safe: the CQE memory is stable until
		// the kernel wraps the ring, which requires us to submit more.
		atomic.StoreUint32(r.m.cqHead, head+1)
		if !yield(cqe) {
			return
		}
	}
}

// Ready reports how many CQEs are currently waiting to be drained.
// Handy for tests and metrics; not needed on the hot path.
func (r *Ring) Ready() uint32 {
	return atomic.LoadUint32(r.m.cqTail) - *r.m.cqHead
}
