// Map the three kernel-shared regions of an io_uring instance:
//   - the submission ring (head/tail indices + index array),
//   - the completion ring (head/tail indices + CQE array),
//   - the SQE array itself.
//
// On modern kernels (>=5.4) the SQ and CQ rings share one mmap; we still
// issue separate mmaps for clarity and so the code tracks the manual
// page one-to-one.

//go:build linux

package iouring

import (
	"fmt"
	"syscall"
	"unsafe"
)

// mapping holds the three shared regions. Every pointer is into
// kernel-owned memory; do not let Go pin or copy it.
type mapping struct {
	sq    []byte // SQ ring (head/tail + idx array)
	cq    []byte // CQ ring (head/tail + CQE array)
	sqes  []byte // array of SQEs, indexed via the SQ idx array

	// Cached pointers into the SQ/CQ regions.
	sqHead    *uint32
	sqTail    *uint32
	sqMask    *uint32
	sqFlags   *uint32
	sqArray   *uint32 // first element; length == *sqMask + 1
	sqEntries uint32

	cqHead    *uint32
	cqTail    *uint32
	cqMask    *uint32
	cqEntries uint32

	sqe       *SQE // first element
	cqe       *CQE // first element
}

const (
	offSQRing  = 0x0
	offCQRing  = 0x8000000
	offSQEs    = 0x10000000
)

func mapRing(fd int, p *Params) (*mapping, error) {
	m := &mapping{
		sqEntries: p.Sq_entries,
		cqEntries: p.Cq_entries,
	}

	sqSize := int(p.Sq_off.Array) + int(p.Sq_entries)*4
	cqSize := int(p.Cq_off.Cqes) + int(p.Cq_entries)*int(unsafe.Sizeof(CQE{}))

	var err error
	m.sq, err = syscall.Mmap(fd, offSQRing, sqSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		return nil, fmt.Errorf("mmap sq: %w", err)
	}

	m.cq, err = syscall.Mmap(fd, offCQRing, cqSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Munmap(m.sq)
		return nil, fmt.Errorf("mmap cq: %w", err)
	}

	sqeBytes := int(p.Sq_entries) * int(unsafe.Sizeof(SQE{}))
	m.sqes, err = syscall.Mmap(fd, offSQEs, sqeBytes,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		syscall.Munmap(m.sq)
		syscall.Munmap(m.cq)
		return nil, fmt.Errorf("mmap sqes: %w", err)
	}

	// Cache the pointers the hot path uses.
	m.sqHead = (*uint32)(unsafe.Pointer(&m.sq[p.Sq_off.Head]))
	m.sqTail = (*uint32)(unsafe.Pointer(&m.sq[p.Sq_off.Tail]))
	m.sqMask = (*uint32)(unsafe.Pointer(&m.sq[p.Sq_off.Ring_mask]))
	m.sqFlags = (*uint32)(unsafe.Pointer(&m.sq[p.Sq_off.Flags]))
	m.sqArray = (*uint32)(unsafe.Pointer(&m.sq[p.Sq_off.Array]))

	m.cqHead = (*uint32)(unsafe.Pointer(&m.cq[p.Cq_off.Head]))
	m.cqTail = (*uint32)(unsafe.Pointer(&m.cq[p.Cq_off.Tail]))
	m.cqMask = (*uint32)(unsafe.Pointer(&m.cq[p.Cq_off.Ring_mask]))

	m.sqe = (*SQE)(unsafe.Pointer(&m.sqes[0]))
	m.cqe = (*CQE)(unsafe.Pointer(&m.cq[p.Cq_off.Cqes]))

	return m, nil
}

func (m *mapping) close() {
	if m.sqes != nil {
		syscall.Munmap(m.sqes)
	}
	if m.cq != nil {
		syscall.Munmap(m.cq)
	}
	if m.sq != nil {
		syscall.Munmap(m.sq)
	}
}

// sqeAt returns the SQE at index i (0..sqEntries-1). Pointer arithmetic
// into the kernel-shared array; do not keep this pointer across ring
// teardown.
func (m *mapping) sqeAt(i uint32) *SQE {
	sz := unsafe.Sizeof(SQE{})
	return (*SQE)(unsafe.Add(unsafe.Pointer(m.sqe), uintptr(i)*sz))
}

// cqeAt returns the CQE at index i (0..cqEntries-1).
func (m *mapping) cqeAt(i uint32) *CQE {
	sz := unsafe.Sizeof(CQE{})
	return (*CQE)(unsafe.Add(unsafe.Pointer(m.cqe), uintptr(i)*sz))
}
