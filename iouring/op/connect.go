// IORING_OP_CONNECT — async connect(2).
//
// Pair with IORING_OP_LINK_TIMEOUT via SQE.Flags |= SQEIoLink to enforce
// a connect timeout without blocking any goroutine. The link timeout
// fires if the connect doesn't complete within the deadline; the kernel
// cancels the connect and both produce CQEs.

//go:build linux

package op

import (
	"unsafe"

	"tachyon/iouring"
)

// Connect arms an async connect to the sockaddr at `addr` (kernel-ABI
// sockaddr struct; length in `addrLen`). Caller is responsible for
// keeping `addr` alive until the CQE arrives.
func Connect(sqe *iouring.SQE, fd int, addr unsafe.Pointer, addrLen uint32, userData uint64) {
	sqe.Opcode = iouring.OpConnect
	sqe.Fd = int32(fd)
	sqe.Addr = uint64(uintptr(addr))
	// CONNECT encodes addrLen in the Off field.
	sqe.Off = uint64(addrLen)
	sqe.UserData = userData
}
