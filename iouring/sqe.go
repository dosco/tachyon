// SQE reservation + submission.
//
// The SQ has two pieces: a ring of indices (sqArray) and a fixed array
// of SQE structs (sqe[]). The flow is:
//
//   1. Reserve an SQE: pick the next slot in the ring (tail & mask) and
//      write its index into sqArray. The SQE itself lives at
//      sqes[tail & mask].
//   2. Fill the SQE's fields (op, fd, buffers, user_data).
//   3. Advance tail with a release store so the kernel sees the new
//      entry.
//   4. When batching is done, the caller submits with Ring.Submit.
//
// SQE prep is split per op under iouring/op/. This file provides the
// reservation primitive and the submit path.

//go:build linux

package iouring

import (
	"errors"
	"sync/atomic"
	"unsafe"
)

// ErrSQFull means the SQ has no free slot; the caller must drain CQEs
// (which frees kernel-side slots) and try again. Never expected in
// steady state — we size the ring above the in-flight ceiling.
var ErrSQFull = errors.New("iouring: submission queue full")

// reserveSQE returns a pointer to the next free SQE and the ring index
// the caller must later publish in sqArray. Does not advance tail —
// that happens in Submit.
//
// Invariant: GOMAXPROCS=1 per worker, so SQ reservation is
// single-threaded; we can read sqHead with a plain load.
func (r *Ring) reserveSQE() (*SQE, uint32, error) {
	// Load head with acquire — kernel writes it.
	head := atomic.LoadUint32(r.m.sqHead)
	tail := r.sqTailShadow
	if tail-head >= r.m.sqEntries {
		return nil, 0, ErrSQFull
	}
	idx := tail & *r.m.sqMask
	sqe := r.m.sqeAt(idx)
	// Zero the SQE — stale fields from the previous use would become
	// kernel requests. unsafe.Slice + range is ~one memset.
	*sqe = SQE{}
	r.sqTailShadow = tail + 1
	r.pendingSubmit++
	// Record the SQE's position so Submit can publish it to sqArray.
	r.pendingIdx[r.pendingSubmit-1] = idx
	return sqe, idx, nil
}

// Submit publishes any reserved SQEs and, if SQPOLL is disabled or has
// gone to sleep, calls io_uring_enter to nudge the kernel. Returns the
// number of SQEs the kernel accepted (== pending before the call, in
// practice).
func (r *Ring) Submit() (int, error) {
	return r.submitAndWait(0)
}

// SubmitAndWait publishes pending SQEs and blocks until at least `min`
// CQEs are ready. Use min=1 for the event-loop wait step; min=0 for
// pure-submission fast path.
func (r *Ring) SubmitAndWait(min uint32) (int, error) {
	return r.submitAndWait(min)
}

func (r *Ring) submitAndWait(min uint32) (int, error) {
	n := r.pendingSubmit
	if n > 0 {
		// Publish each reserved SQE's index into sqArray at the
		// pre-advance tail. The kernel reads sqArray[tail & mask] to
		// find the SQE to run.
		start := r.sqTailShadow - n
		mask := *r.m.sqMask
		for i := uint32(0); i < n; i++ {
			slot := (start + i) & mask
			*(*uint32)(unsafe.Add(unsafe.Pointer(r.m.sqArray), uintptr(slot)*4)) = r.pendingIdx[i]
		}
		// Release store of tail — the kernel does an acquire load.
		atomic.StoreUint32(r.m.sqTail, r.sqTailShadow)
		r.pendingSubmit = 0
	}

	// Fast path: SQPOLL, kernel thread awake, nothing to wait for.
	if r.sqpoll && !r.needsWakeup() && min == 0 {
		return int(n), nil
	}

	var flags uint32
	if r.sqpoll && r.needsWakeup() {
		flags |= EnterSQWakeup
	}
	if min > 0 {
		flags |= EnterGetEvents
	}
	submitted := uint32(n)
	if r.sqpoll {
		// With SQPOLL the kernel consumes the SQ itself; we pass 0 for
		// toSubmit so enter only serves as wake / wait.
		submitted = 0
	}
	return Enter(r.fd, submitted, min, flags)
}

// needsWakeup reports whether the SQPOLL kernel thread has gone idle
// and must be kicked. Only meaningful when sqpoll=true.
func (r *Ring) needsWakeup() bool {
	return atomic.LoadUint32(r.m.sqFlags)&SQNeedWakeup != 0
}
