// IORING_OP_ACCEPT — kernel-side accept(4).
//
// Multishot accept (flag IORING_ACCEPT_MULTISHOT, Linux 5.19+) is what
// tachyon uses: one SQE arms the kernel to produce a fresh CQE for every
// accepted connection until we RST_STREAM or the fd is closed. No need
// to re-arm per connection.
//
// Completion:
//   - cqe.Res >= 0 : accepted fd
//   - cqe.Res < 0  : -errno
//   - cqe.Flags & CQEFMore == 0 signals the multishot armed op was torn
//     down (e.g. listener closed); the caller must re-arm.

//go:build linux

package op

import (
	"unsafe"

	"tachyon/iouring"
)

// AcceptMultishot arms a multishot accept on listenFD. userData tags the
// resulting CQEs so the dispatcher can route them. addr/addrlen are
// optional: pass (nil, nil) to ignore the peer address.
//
// The SQE passed in is one previously returned from Ring.Reserve; this
// function fills it in place.
func AcceptMultishot(sqe *iouring.SQE, listenFD int, userData uint64) {
	sqe.Opcode = iouring.OpAccept
	sqe.Fd = int32(listenFD)
	sqe.UserData = userData
	// IORING_ACCEPT_MULTISHOT lives in ioprio for the ACCEPT op.
	sqe.IOPrio = iouring.AcceptMultishot
}

// Accept (one-shot) variant; useful for tests. SOCK_CLOEXEC|SOCK_NONBLOCK
// is a reasonable default to pass via flags.
func Accept(sqe *iouring.SQE, listenFD int, addr unsafe.Pointer, addrLen *uint32, flags uint32, userData uint64) {
	sqe.Opcode = iouring.OpAccept
	sqe.Fd = int32(listenFD)
	sqe.Addr = uint64(uintptr(addr))
	sqe.Off = uint64(uintptr(unsafe.Pointer(addrLen)))
	sqe.OpFlags = flags
	sqe.UserData = userData
}
