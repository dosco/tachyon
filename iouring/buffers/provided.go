// REGISTER_PBUF_RING — provided buffer rings (Linux 5.19+).
//
// Classic recv requires the caller to hand the kernel a buffer up front.
// Provided buffers let the kernel pick a buffer out of a pre-registered
// ring on arrival: we register N buffers, the kernel draws from them as
// packets arrive, and tells us which one it used via CQE.flags. That
// means:
//
//   - No "which conn will get traffic next" guessing — one shared pool
//     serves every connection.
//   - No buffer is tied up waiting for data that may never come.
//   - No per-recv syscall to refresh buffers; the ring is mmap'd and we
//     recycle by bumping a tail pointer.
//
// This is the single biggest recv-path win io_uring offers, and it's
// the reason tachyon rolls its own binding rather than using one of the
// existing Go io_uring libraries.

//go:build linux

package buffers

import (
	"errors"
	"fmt"
	"syscall"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
	"tachyon/iouring"
)

// bufRingEntry mirrors struct io_uring_buf (used as an array element in
// the mmap'd buffer ring).
type bufRingEntry struct {
	Addr uint64 // pointer to buffer
	Len  uint32 // buffer size
	BID  uint16 // caller-chosen id; surfaces in CQE.flags on selection
	Resv uint16
}

// bufRingRegReq mirrors struct io_uring_buf_reg (the argument to
// REGISTER_PBUF_RING).
type bufRingRegReq struct {
	RingAddr    uint64
	RingEntries uint32
	BgID        uint16
	Flags       uint16
	Resv        [3]uint64
}

// Ring is a registered provided-buffer ring.
//
// Lifecycle:
//   1. Provide()    — allocate + MMAP the ring, register it with the kernel.
//   2. Recycle(id)  — after processing a CQE that used buffer id, hand
//                     the slab back so the kernel can pick it again.
//   3. Close()      — unregister and free.
type Ring struct {
	ringAddr unsafe.Pointer // start of the mmap'd ring array
	slab     []byte         // contiguous slab backing every buffer
	bufSize  int
	count    uint16
	bgID     uint16
	mask     uint16
	tail     uint16 // local bump counter; published via atomic store to the kernel-visible tail
	ringFD   int
	uringFD  int
}

// tailOff: the kernel treats the first ring entry as a header aliased
// with a plain buf entry. The tail u16 sits at offset 14 of entry[0],
// i.e. where the entry's `resv` field is. addr/len/bid are left intact,
// which is why slot 0 is also a usable buffer.
const tailOff = 14

// Provide creates count buffers of bufSize each, grouped under bgID.
// Pass ring.FD() as uringFD. The ring uses a mmap'd array; every entry
// is a bufRingEntry, with entry[0].Addr repurposed as the kernel tail
// counter.
func Provide(uringFD int, bgID uint16, count uint16, bufSize int) (*Ring, error) {
	if count == 0 || count&(count-1) != 0 {
		return nil, errors.New("buffers: count must be a power of two")
	}
	ringBytes := int(count) * int(unsafe.Sizeof(bufRingEntry{}))
	// Allocate via mmap so it's page-aligned (kernel requires).
	m, err := syscall.Mmap(-1, 0, ringBytes,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("buffers: mmap ring: %w", err)
	}
	// Slab backing the buffers themselves.
	slab := make([]byte, int(count)*bufSize)

	r := &Ring{
		ringAddr: unsafe.Pointer(&m[0]),
		slab:     slab,
		bufSize:  bufSize,
		count:    count,
		bgID:     bgID,
		mask:     count - 1,
		uringFD:  uringFD,
	}

	// Register the ring with the kernel.
	req := bufRingRegReq{
		RingAddr:    uint64(uintptr(r.ringAddr)),
		RingEntries: uint32(count),
		BgID:        bgID,
	}
	if _, err := iouring.Register(uringFD, iouring.RegisterPbufRing,
		unsafe.Pointer(&req), 1); err != nil {
		syscall.Munmap(m)
		return nil, fmt.Errorf("buffers: REGISTER_PBUF_RING: %w", err)
	}

	// Populate every slot with a buffer. The kernel advances an
	// internal head as it picks buffers; our tail starts at count
	// (everything available).
	for i := uint16(0); i < count; i++ {
		r.putSlot(i, i)
	}
	// Publish initial tail to the kernel.
	r.publishTail(count)
	r.tail = count

	return r, nil
}

// putSlot writes buffer `bid` into ring slot `slot`.
func (r *Ring) putSlot(slot, bid uint16) {
	entry := r.entryAt(slot)
	entry.Addr = uint64(uintptr(unsafe.Pointer(&r.slab[int(bid)*r.bufSize])))
	entry.Len = uint32(r.bufSize)
	entry.BID = bid
}

// publishTail writes the kernel-visible tail (stored in entry[0].Addr's
// low 16 bits per the kernel ABI — actually a dedicated u16 field at
// offset 8 of the ring header on current kernels; we use the simple
// u16 layout which io_uring_setup_buf_ring in liburing uses).
func (r *Ring) publishTail(v uint16) {
	// Atomic u16 store at offset 14 of entry[0]. Go stdlib doesn't have
	// StoreUint16; the kernel reads it with smp_load_acquire(u16*) so
	// a plain aligned u16 store with a memory barrier suffices. We use
	// a StoreUint32 on the surrounding u32 — but that'd clobber adjacent
	// data. Instead: plain store + a full barrier via atomic on another
	// word.
	p := (*uint16)(unsafe.Add(r.ringAddr, uintptr(tailOff)))
	*p = v
	// Release fence: a dummy atomic op on any word pairs with the
	// kernel's acquire load of tail.
	atomic.AddUint32((*uint32)(unsafe.Add(r.ringAddr, 0)), 0)
}

// entryAt returns the ring entry at slot (wrapped).
func (r *Ring) entryAt(slot uint16) *bufRingEntry {
	sz := unsafe.Sizeof(bufRingEntry{})
	return (*bufRingEntry)(unsafe.Add(r.ringAddr, uintptr(slot)*sz))
}

// GroupID returns the buffer group ID used when arming recv.
func (r *Ring) GroupID() uint16 { return r.bgID }

// BufferSize returns the fixed per-buffer size.
func (r *Ring) BufferSize() int { return r.bufSize }

// Bytes returns the payload of buffer `bid`, sized to `n` bytes (as
// reported by CQE.Res).
func (r *Ring) Bytes(bid uint16, n int) []byte {
	off := int(bid) * r.bufSize
	return r.slab[off : off+n]
}

// Recycle returns buffer `bid` to the ring so the kernel can pick it
// again. Hot path — called once per processed recv.
func (r *Ring) Recycle(bid uint16) {
	slot := r.tail & r.mask
	r.putSlot(slot, bid)
	r.tail++
	r.publishTail(r.tail)
}

// Close unregisters and unmaps the ring.
func (r *Ring) Close() error {
	req := bufRingRegReq{BgID: r.bgID}
	if _, err := iouring.Register(r.uringFD, iouring.UnregisterPbufRing,
		unsafe.Pointer(&req), 1); err != nil && err != unix.ENOENT {
		return err
	}
	sl := unsafe.Slice((*byte)(r.ringAddr),
		int(r.count)*int(unsafe.Sizeof(bufRingEntry{})))
	return syscall.Munmap(sl)
}
