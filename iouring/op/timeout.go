// IORING_OP_TIMEOUT — expire a CQE after a relative or absolute
// duration. tachyon uses it as the idle-connection reaper: one armed
// timeout per idle upstream. No timer goroutine, no heap.
//
// Completion:
//   - cqe.Res == -ETIME : timer fired as scheduled (the normal case)
//   - cqe.Res ==  0     : timer was cancelled via OpAsyncCancel
//   - cqe.Res <  0      : other error

//go:build linux

package op

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"tachyon/iouring"
)

// TimeoutFlagAbs means ts is an absolute kernel time; without it, ts is
// a relative duration from now.
const (
	TimeoutFlagAbs     uint32 = 1 << 0
	TimeoutFlagBoottime uint32 = 1 << 2
	TimeoutFlagRealtime uint32 = 1 << 3
)

// Timeout arms a timer that fires after `ts`. Pass flags=0 for a
// relative duration. The Timespec must remain alive until the CQE
// arrives.
func Timeout(sqe *iouring.SQE, ts *unix.Timespec, flags uint32, userData uint64) {
	sqe.Opcode = iouring.OpTimeout
	sqe.Addr = uint64(uintptr(unsafe.Pointer(ts)))
	sqe.Len = 1 // TIMEOUT's Len is "number of completions after which to fire"; 0 is special (fire only on timeout), 1 fires after the first event OR the timeout — we want pure timeout, so set Len=1 with the single-entry semantics the kernel uses.
	sqe.OpFlags = flags
	sqe.UserData = userData
}
