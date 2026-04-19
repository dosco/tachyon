// io_uring_setup(2) wrapper.
//
// Creates a new io_uring instance and returns the ring fd plus the
// kernel-filled parameter struct describing ring sizes and offsets.
//
// The returned fd is what later gets mmap'd (see mmap.go) and passed to
// io_uring_enter / io_uring_register.

//go:build linux

package iouring

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Setup invokes io_uring_setup(entries, &p). entries is the requested SQ
// size; the kernel rounds up to a power of two and may clamp. On return
// p is populated with the actual ring geometry (sq/cq entries, offsets,
// features).
//
// The returned fd must be closed with unix.Close when the ring is torn
// down.
func Setup(entries uint32, p *Params) (int, error) {
	r1, _, errno := syscall.Syscall(
		uintptr(unix.SYS_IO_URING_SETUP),
		uintptr(entries),
		uintptr(unsafe.Pointer(p)),
		0,
	)
	if errno != 0 {
		return -1, fmt.Errorf("io_uring_setup: %w", errno)
	}
	return int(r1), nil
}
