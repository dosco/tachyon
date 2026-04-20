//go:build linux

package main

import (
	"bytes"
	"strconv"

	"golang.org/x/sys/unix"

	"tachyon/iouring"
)

// uringCaps summarizes what the running kernel actually supports. Filled
// in once at startup by probeUringCaps; consumed by the resolve-io-mode
// logic that picks between stdlib and uring for `-io auto`.
//
// The intent is a single, honest "can we safely default to uring?"
// answer. Individual features are recorded for future per-feature gates
// (e.g. "run SPLICE path only if ≥6.0") without having to re-probe.
type uringCaps struct {
	setupOK  bool  // io_uring_setup(8, 0) succeeded
	kernel   [3]int // major, minor, patch; zero on parse failure
	// Feature flags — all derived from the kernel version because
	// individual IORING_FEAT bits weren't widely plumbed through the Go
	// wrapper yet. Version gating is coarser than feature-probing but
	// accurate enough for the default-selection question.
	hasMultishotRecv bool // 6.0+
	hasSendZC        bool // 6.0+
	hasSplice        bool // 5.7+
	hasPBufRing      bool // 5.19+
	hasDeferTaskrun  bool // 6.1+
}

// probeUring is kept as the minimal capability check (can we build a
// ring at all?). Used by resolveIOMode alongside probeUringCaps.
func probeUring() bool {
	r, err := iouring.New(8, 0)
	if err != nil {
		return false
	}
	_ = r.Close()
	return true
}

// probeUringCaps runs the full detection. Cheap: one uname(2) + one
// io_uring_setup(8, 0).
func probeUringCaps() uringCaps {
	var c uringCaps
	c.setupOK = probeUring()
	if !c.setupOK {
		return c
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		c.kernel = parseKernelVersion(u.Release[:])
	}
	major, minor := c.kernel[0], c.kernel[1]
	atLeast := func(mj, mi int) bool {
		if major > mj {
			return true
		}
		return major == mj && minor >= mi
	}
	c.hasSplice = atLeast(5, 7)
	c.hasPBufRing = atLeast(5, 19)
	c.hasMultishotRecv = atLeast(6, 0)
	c.hasSendZC = atLeast(6, 0)
	c.hasDeferTaskrun = atLeast(6, 1)
	return c
}

// parseKernelVersion extracts the leading "X.Y.Z" from a utsname release
// string like "6.17.0-1010-gcp". Null terminator tolerated. Returns
// zeros on a malformed string — callers then see all "has*" features as
// false and fall back to stdlib.
func parseKernelVersion(rel []byte) [3]int {
	if i := bytes.IndexByte(rel, 0); i >= 0 {
		rel = rel[:i]
	}
	var out [3]int
	for idx := 0; idx < 3; idx++ {
		j := 0
		for j < len(rel) && rel[j] >= '0' && rel[j] <= '9' {
			j++
		}
		if j == 0 {
			return out
		}
		n, err := strconv.Atoi(string(rel[:j]))
		if err != nil {
			return out
		}
		out[idx] = n
		rel = rel[j:]
		if len(rel) == 0 || (rel[0] != '.' && rel[0] != '-') {
			return out
		}
		if rel[0] == '-' {
			return out
		}
		rel = rel[1:]
	}
	return out
}

// resolveIOMode maps the user's -io choice and kernel caps into the final
// "use uring?" decision. Called from main once per worker.
//
// Resolution table:
//
//	-io uring    → uring (forced; kernel rejection is a fatal error)
//	-io std      → stdlib
//	-io auto     → uring if kernel ≥5.7 (hasSplice) and ring setup OK
//	               stdlib otherwise (older kernel, non-Linux, setup failed)
//
// Rationale: cross-VM benchmarks show io_uring wins on every real-network
// scenario — +4–10 % on small bodies, +2–3 % on large bodies. The only
// case where stdlib wins is loopback (proxy and upstream on the same
// machine), where the epoll model is 6–10 % faster because there is no
// network latency to amortize. Production is never loopback; use -io=std
// explicitly if you need the loopback-optimal path.
func resolveIOMode(ioFlag string, caps uringCaps) bool {
	switch ioFlag {
	case "uring":
		return true
	case "std":
		return false
	}
	// auto: uring on any kernel that supports SPLICE (≥5.7) and can set
	// up a ring. Falls back to stdlib gracefully on older kernels.
	return caps.setupOK && caps.hasSplice
}
