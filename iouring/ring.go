// Ring ties setup + mmap + sqe + cqe + enter into one handle.
//
// A worker owns exactly one Ring. It is not safe for concurrent use
// (by design — we rely on GOMAXPROCS=1 per worker to avoid atomics on
// the SQ reservation path).

//go:build linux

package iouring

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Ring is a worker's handle to an io_uring instance.
type Ring struct {
	fd     int
	params Params
	m      *mapping
	sqpoll bool

	// sqTailShadow is our local copy of the SQ tail; we publish it to
	// the kernel-visible tail on Submit. Avoids a release store per
	// reserved SQE.
	sqTailShadow uint32

	// pendingSubmit counts SQEs reserved but not yet published.
	pendingSubmit uint32
	pendingIdx    []uint32 // [i] = SQE array index for the i-th pending reservation
}

// New creates a Ring with `entries` submission slots. `flags` are
// IORING_SETUP_* bits (see setup.go). Typical production call:
//
//	r, err := iouring.New(4096, iouring.SetupClamp|iouring.SetupSingleIssuer)
//
// Add SetupSQPoll for zero-syscall steady state if you want the
// trade-off (one kernel thread per worker).
func New(entries uint32, flags uint32) (*Ring, error) {
	var p Params
	p.Flags = flags
	fd, err := Setup(entries, &p)
	if err != nil {
		return nil, err
	}
	m, err := mapRing(fd, &p)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("iouring: map: %w", err)
	}
	r := &Ring{
		fd:         fd,
		params:     p,
		m:          m,
		sqpoll:     flags&SetupSQPoll != 0,
		pendingIdx: make([]uint32, p.Sq_entries),
	}
	return r, nil
}

// FD returns the ring fd. Exposed so that register-ops implemented in
// sub-packages (iouring/buffers) can call io_uring_register on this
// ring without wrapping every op here.
func (r *Ring) FD() int { return r.fd }

// Params returns the kernel-reported parameters. Useful for feature
// detection (Features bits) and sizing.
func (r *Ring) ParamsCopy() Params { return r.params }

// Reserve returns a zeroed SQE ready for one op to fill. The returned
// pointer is valid only until the next Reserve / Submit. Caller must
// always eventually Submit — reserved-but-unpublished SQEs hold the SQ
// full.
func (r *Ring) Reserve() (*SQE, error) {
	sqe, _, err := r.reserveSQE()
	return sqe, err
}

// Close tears the ring down. All outstanding operations are cancelled
// by the kernel.
func (r *Ring) Close() error {
	r.m.close()
	return unix.Close(r.fd)
}
