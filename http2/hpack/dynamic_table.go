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

	// Hash table sized 4× entry cap keeps load factor ≤ 25%, which
	// keeps expected probe length < 1.5 on lookups. Must be a power
	// of two so `hash & mask` works.
	dynHashCap  = 1024
	dynHashMask = dynHashCap - 1

	// hashEmpty vs hashTomb distinguish never-used buckets from
	// buckets freed by eviction. Linear probing needs tombstones so
	// the probe chain isn't broken by a hole.
	hashEmpty = uint16(0)
	hashTomb  = uint16(0xFFFF)
)

type entryRef struct {
	nameOff uint16
	nameLen uint16
	valOff  uint16
	valLen  uint16
}

// DynamicTable is the HPACK dynamic table. Zero value is NOT ready-to-use;
// construct with NewDynamicTable.
//
// nameIdx / exactIdx are open-addressed hash tables keyed by FNV-1a of
// the header name (resp. name+value). Each bucket stores slot+1 so
// hashEmpty (0) is distinguishable from slot 0. Lookups are O(1)
// expected; without the index, every header encode paid an O(n) linear
// scan of up to dynEntriesCap entries.
type DynamicTable struct {
	arena    [dynArenaCap]byte
	arenaLen int
	entries  [dynEntriesCap]entryRef
	nameIdx  [dynHashCap]uint16 // FNV(name) → slot+1 (0=empty, 0xFFFF=tombstone)
	exactIdx [dynHashCap]uint16 // FNV(name|0|value) → slot+1
	// head is the slot index where the NEXT insertion will go.
	// The newest entry lives at (head-1) mod dynEntriesCap.
	head    int
	count   int
	size    int
	maxSize int
}

// fnv1aName hashes just the name. Mirrors standard FNV-1a (32-bit) but
// manually inlined to avoid the hash/fnv allocation.
func fnv1aName(b []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range b {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

// fnv1aExact hashes name, a 0x00 separator, then value. Keeps
// ("x","y") distinct from ("xy","") etc.
func fnv1aExact(name, value []byte) uint32 {
	h := uint32(2166136261)
	for _, c := range name {
		h ^= uint32(c)
		h *= 16777619
	}
	h ^= 0
	h *= 16777619
	for _, c := range value {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
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

// slotTo1Based inverts slot(): given a ring slot, return the 1-based
// RFC 7541 dynamic-table index (1 = newest). Returns 0 if the slot
// isn't currently part of the live window.
func (d *DynamicTable) slotTo1Based(s int) int {
	// (head-1-i) % N = s → i = (head-1-s) % N
	i := (d.head - 1 - s + dynEntriesCap*2) % dynEntriesCap
	if i < 0 || i >= d.count {
		return 0
	}
	return i + 1
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

// hashInsert places `slot+1` at the first non-empty/non-matching
// bucket starting at the hash. Caller supplies which hash table via
// the table pointer.
func (d *DynamicTable) hashInsert(tbl *[dynHashCap]uint16, hash uint32, slot int) {
	i := hash & dynHashMask
	for k := 0; k < dynHashCap; k++ {
		v := tbl[i]
		if v == hashEmpty || v == hashTomb {
			tbl[i] = uint16(slot + 1)
			return
		}
		// Duplicate: overwrite so newest-wins semantics hold
		// for lookup-by-name. Caller guarantees slot is distinct
		// only when it actually wants a fresh entry; for
		// duplicates we want to point at the newer slot.
		if int(v-1) == slot {
			return
		}
		i = (i + 1) & dynHashMask
	}
}

// hashRemove clears the bucket whose stored value is slot+1, walking
// from the given hash. Uses a tombstone so the probe chain stays
// intact.
func (d *DynamicTable) hashRemove(tbl *[dynHashCap]uint16, hash uint32, slot int) {
	target := uint16(slot + 1)
	i := hash & dynHashMask
	for k := 0; k < dynHashCap; k++ {
		v := tbl[i]
		if v == hashEmpty {
			return
		}
		if v == target {
			tbl[i] = hashTomb
			return
		}
		i = (i + 1) & dynHashMask
	}
}

// hashFindName probes nameIdx for a bucket whose slot has matching
// bytes. Returns the slot (0-based ring index) or -1 on miss.
func (d *DynamicTable) hashFindName(name []byte) int {
	hash := fnv1aName(name)
	i := hash & dynHashMask
	for k := 0; k < dynHashCap; k++ {
		v := d.nameIdx[i]
		if v == hashEmpty {
			return -1
		}
		if v != hashTomb {
			slot := int(v - 1)
			e := d.entries[slot]
			if int(e.nameLen) == len(name) {
				n := d.arena[e.nameOff : e.nameOff+e.nameLen]
				if bytesEqRaw(n, name) {
					return slot
				}
			}
		}
		i = (i + 1) & dynHashMask
	}
	return -1
}

// hashFindExact mirrors hashFindName but matches both name and value.
func (d *DynamicTable) hashFindExact(name, value []byte) int {
	hash := fnv1aExact(name, value)
	i := hash & dynHashMask
	for k := 0; k < dynHashCap; k++ {
		v := d.exactIdx[i]
		if v == hashEmpty {
			return -1
		}
		if v != hashTomb {
			slot := int(v - 1)
			e := d.entries[slot]
			if int(e.nameLen) == len(name) && int(e.valLen) == len(value) {
				n := d.arena[e.nameOff : e.nameOff+e.nameLen]
				vv := d.arena[e.valOff : e.valOff+e.valLen]
				if bytesEqRaw(n, name) && bytesEqRaw(vv, value) {
					return slot
				}
			}
		}
		i = (i + 1) & dynHashMask
	}
	return -1
}

// bytesEqRaw is a local copy to avoid an exported-surface dep loop.
// Mirrors encoder.bytesEq.
func bytesEqRaw(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// evictOldest drops the single oldest entry.
func (d *DynamicTable) evictOldest() {
	if d.count == 0 {
		return
	}
	s := d.oldestSlot()
	e := d.entries[s]
	// Remove hash entries before we invalidate the arena offsets.
	name := d.arena[e.nameOff : e.nameOff+e.nameLen]
	value := d.arena[e.valOff : e.valOff+e.valLen]
	d.hashRemove(&d.nameIdx, fnv1aName(name), s)
	d.hashRemove(&d.exactIdx, fnv1aExact(name, value), s)
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
		// All entries gone; reclaim the arena and the hash slots.
		// Clearing hashes here (vs leaving tombstones) keeps probe
		// chains from accumulating across a long run of churn.
		d.arenaLen = 0
		d.resetHashes()
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
		d.resetHashes()
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

	slot := d.head
	d.entries[slot] = entryRef{
		nameOff: uint16(nameOff),
		nameLen: uint16(len(name)),
		valOff:  uint16(valOff),
		valLen:  uint16(len(value)),
	}
	// Hash indices point at the *slot*, not arena offsets, so they
	// survive compact() without rebuild. Insert after the entry is
	// live so probes see valid data.
	d.hashInsert(&d.nameIdx, fnv1aName(name), slot)
	d.hashInsert(&d.exactIdx, fnv1aExact(name, value), slot)

	d.head = (d.head + 1) % dynEntriesCap
	d.count++
	d.size += entrySize
}

// resetHashes zeros both hash tables. Called when the table empties
// (via SetMaxSize too small, or an oversized Add) so tombstones don't
// accumulate forever.
func (d *DynamicTable) resetHashes() {
	for i := range d.nameIdx {
		d.nameIdx[i] = hashEmpty
	}
	for i := range d.exactIdx {
		d.exactIdx[i] = hashEmpty
	}
}
