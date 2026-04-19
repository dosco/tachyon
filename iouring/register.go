// io_uring_register(2) generic wrapper.
//
// Register is the kernel's "install this resource ahead of time" knob:
// fixed buffers (REGISTER_BUFFERS), fixed files (REGISTER_FILES), and
// the provided-buffer ring (REGISTER_PBUF_RING) that makes recv
// zero-copy. Specific wrappers live in iouring/buffers/.

//go:build linux

package iouring

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Register invokes io_uring_register(fd, op, arg, nr).
func Register(fd int, op uint32, arg unsafe.Pointer, nr uint32) (int, error) {
	r1, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_IO_URING_REGISTER),
		uintptr(fd),
		uintptr(op),
		uintptr(arg),
		uintptr(nr),
		0, 0,
	)
	if errno != 0 {
		return int(r1), errno
	}
	return int(r1), nil
}
