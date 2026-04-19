// io_uring_enter(2) wrapper.
//
// The kernel's front door for submitting SQEs and/or waiting for CQEs.
// With SQPOLL enabled the kernel polls the SQ on its own and this
// becomes a no-op in the steady state — we only call Enter when the SQ
// needs a wake-up or the user requested a blocking wait.

//go:build linux

package iouring

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Enter calls io_uring_enter with no sigset. toSubmit is the number of
// new SQEs the caller has prepared; minComplete is the number of CQEs
// the kernel should wait for before returning (0 = non-blocking).
func Enter(fd int, toSubmit, minComplete, flags uint32) (int, error) {
	r1, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_IO_URING_ENTER),
		uintptr(fd),
		uintptr(toSubmit),
		uintptr(minComplete),
		uintptr(flags),
		0, // sigset
		0,
	)
	if errno != 0 && errno != syscall.EINTR {
		return int(r1), errno
	}
	return int(r1), nil
}

// EnterTimeout calls io_uring_enter with a kernel-absolute timespec as
// the "extra arg" (EXT_ARG). Used by the event loop to bound wait time
// when no idle timers are armed otherwise.
func EnterTimeout(fd int, toSubmit, minComplete, flags uint32, ts *unix.Timespec) (int, error) {
	type getEventsArg struct {
		sigmask   uint64
		sigmaskSz uint32
		pad       uint32
		ts        uint64
	}
	arg := getEventsArg{ts: uint64(uintptr(unsafe.Pointer(ts)))}
	r1, _, errno := syscall.Syscall6(
		uintptr(unix.SYS_IO_URING_ENTER),
		uintptr(fd),
		uintptr(toSubmit),
		uintptr(minComplete),
		uintptr(flags|EnterExtArg),
		uintptr(unsafe.Pointer(&arg)),
		unsafe.Sizeof(arg),
	)
	if errno != 0 && errno != syscall.EINTR && errno != syscall.ETIME {
		return int(r1), errno
	}
	return int(r1), nil
}
