//go:build linux

package runtime

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// PinToCPU locks the calling OS thread to the given CPU. Combined with
// GOMAXPROCS=1 on the worker process, this prevents the Go scheduler from
// ever migrating our one runnable goroutine off its core - which in turn
// makes the per-worker upstream HotPool trivially safe to use without locks.
//
// Returns any error from sched_setaffinity; callers log and continue.
func PinToCPU(cpu int) error {
	runtime.LockOSThread()
	var set unix.CPUSet
	set.Zero()
	set.Set(cpu)
	return unix.SchedSetaffinity(0, &set)
}
