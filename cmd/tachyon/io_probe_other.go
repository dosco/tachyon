//go:build !linux

package main

// uringCaps mirrors the linux definition so non-linux builds compile.
// Every field is false / zero — io_uring doesn't exist off Linux.
type uringCaps struct {
	setupOK          bool
	kernel           [3]int
	hasMultishotRecv bool
	hasSendZC        bool
	hasSplice        bool
	hasPBufRing      bool
	hasDeferTaskrun  bool
}

// probeUring always returns false on non-Linux: io_uring is a Linux kernel
// feature. The server will serve on the stdlib event loop.
func probeUring() bool { return false }

// probeUringCaps is a no-op on non-Linux; returns zero-value caps.
func probeUringCaps() uringCaps { return uringCaps{} }

// resolveIOMode on non-Linux always picks stdlib. Mirrors the Linux
// signature so main doesn't need a build-tagged call site.
func resolveIOMode(ioFlag string, _ uringCaps) bool {
	return ioFlag == "uring" // forced mode still attempts; will fail cleanly
}
