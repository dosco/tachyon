// dynamic_table.go: HPACK dynamic table, zero-allocation after construction.
//
// Design choice: ring of entry references into a fixed byte arena. The
// arena is NOT a linear ring — we eschew the cross-boundary wrap complexity
// by using a "bump-then-reset" allocator: when an incoming entry's bytes
// won't fit in the contiguous tail, we copy all live entries' bytes down to
// offset 0 in place, compacting. Entries are identified by (off, len) into
// the arena. Compaction is at most one memcpy of <= maxSize bytes per
// insertion that wraps, amortized cheap and still allocation-free.
//
// Per RFC 7541 §4.4, an entry larger than maxSize causes the entire table
// to empty and the entry is dropped (Add becomes a no-op).
//
// Entry indexing: RFC 7541 indexes dynamic entries starting at 1, with the
// most-recent insertion at index 1. Our ring stores newest at head-1
// (mod cap). Get translates the 1-based spec index into the ring slot.

package hpack

const (
	dynArenaCap   = 4096
	dynEntriesCap = 256
	// hpackEntryOverhead is the per-entry size constant defined by
	// RFC 7541 §4.1: size = len(name) + len(value) + 32.
	hpackEntryOverhead = 32
)

type entryRef struct {
	nameOff uint16
	nameLen uint16
	valOff  uint16
	valLen  uint16
}

// DynamicTable is the HPACK dynamic table. Zero value is NOT ready-to-use;
// construct with NewDynamicTable.
type DynamicTable struct {
	arena    [dynArenaCap]byte
	arenaLen int
	entries  [dynEntriesCap]entryRef
	// head is the slot index where the NEXT insertion will go.
	// The newest entry lives at (head-1) mod dynEntriesCap.
	head    int
	count   int
	size    int
	maxSize int
}

// NewDynamicTable returns a table with the given maximum size in bytes.
// maxSize may be adjusted later via SetMaxSize.
func NewDynamicTable(maxSize int) *DynamicTable {
	if maxSize > dynArenaCap {
		maxSize = dynArenaCap
	}
	return &DynamicTable{maxSize: maxSize}
}

// Len returns the number of entries currently in the dynamic table.
func (d *DynamicTable) Len() int { return d.count }

// Size returns the current HPACK-defined size (RFC 7541 §4.1).
func (d *DynamicTable) Size() int { return d.size }

// MaxSize returns the configured maximum table size.
func (d *DynamicTable) MaxSize() int { return d.maxSize }

// SetMaxSize changes the maximum size. Entries are evicted until size fits.
func (d *DynamicTable) SetMaxSize(n int) {
	if n > dynArenaCap {
		n = dynArenaCap
	}
	d.maxSize = n
	d.evictUntilFits(0)
}

// slot returns the ring slot holding the i-th newest entry (i is 0-based).
func (d *DynamicTable) slot(i int) int {
	return (d.head - 1 - i + dynEntriesCap*2) % dynEntriesCap
}

// oldestSlot returns the ring slot of the oldest entry.
func (d *DynamicTable) oldestSlot() int {
	return (d.head - d.count + dynEntriesCap*2) % dynEntriesCap
}

// Get returns (name, value) at 1-based dynamic-table index idx. Returned
// slices alias the internal arena and remain valid only until the next
// mutation.
func (d *DynamicTable) Get(idx int) (name, value []byte, ok bool) {
	if idx < 1 || idx > d.count {
		return nil, nil, false
	}
	e := d.entries[d.slot(idx-1)]
	return d.arena[e.nameOff : e.nameOff+e.nameLen],
		d.arena[e.valOff : e.valOff+e.valLen], true
}

// evictOldest drops the single oldest entry.
func (d *DynamicTable) evictOldest() {
	if d.count == 0 {
		return
	}
	s := d.oldestSlot()
	e := d.entries[s]
	d.size -= int(e.nameLen) + int(e.valLen) + hpackEntryOverhead
	d.entries[s] = entryRef{}
	d.count--
}

// evictUntilFits removes oldest entries until size+want <= maxSize.
func (d *DynamicTable) evictUntilFits(want int) {
	for d.count > 0 && d.size+want > d.maxSize {
		d.evictOldest()
	}
	if d.count == 0 {
		// All entries gone; reclaim the arena.
		d.arenaLen = 0
	}
}

// compact copies all live entry bytes to the start of the arena,
// rewriting their offsets. Used when the contiguous tail is too small but
// total free space suffices.
func (d *DynamicTable) compact() {
	var scratch [dynArenaCap]byte
	newLen := 0
	for i := 0; i < d.count; i++ {
		// Walk from oldest to newest to keep relative order.
		s := (d.oldestSlot() + i) % dynEntriesCap
		e := &d.entries[s]
		copy(scratch[newLen:], d.arena[e.nameOff:e.nameOff+e.nameLen])
		e.nameOff = uint16(newLen)
		newLen += int(e.nameLen)
		copy(scratch[newLen:], d.arena[e.valOff:e.valOff+e.valLen])
		e.valOff = uint16(newLen)
		newLen += int(e.valLen)
	}
	copy(d.arena[:newLen], scratch[:newLen])
	d.arenaLen = newLen
}

// Add inserts (name, value) as the newest entry. Evicts older entries as
// required. If the entry alone exceeds maxSize the table is emptied and
// the entry is dropped (RFC 7541 §4.4).
func (d *DynamicTable) Add(name, value []byte) {
	entrySize := len(name) + len(value) + hpackEntryOverhead
	if entrySize > d.maxSize {
		// Empty table and drop.
		for d.count > 0 {
			d.evictOldest()
		}
		d.arenaLen = 0
		return
	}
	d.evictUntilFits(entrySize)

	need := len(name) + len(value)
	if d.arenaLen+need > dynArenaCap {
		d.compact()
	}
	// After compact() the tail space equals dynArenaCap - size. Since
	// entrySize <= maxSize <= dynArenaCap and all other entries total
	// size-entrySize <= maxSize - entrySize, we're guaranteed to fit.

	nameOff := d.arenaLen
	copy(d.arena[nameOff:], name)
	d.arenaLen += len(name)
	valOff := d.arenaLen
	copy(d.arena[valOff:], value)
	d.arenaLen += len(value)

	d.entries[d.head] = entryRef{
		nameOff: uint16(nameOff),
		nameLen: uint16(len(name)),
		valOff:  uint16(valOff),
		valLen:  uint16(len(value)),
	}
	d.head = (d.head + 1) % dynEntriesCap
	d.count++
	d.size += entrySize
}
