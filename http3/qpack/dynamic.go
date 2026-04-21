// Dynamic-table support for QPACK (RFC 9204 §3.2).
//
// This file adds the server-side decoder pieces needed to accept
// dynamic-table references from peer encoders:
//
//   - A ring-buffer DynamicTable with per-RFC size accounting
//     (name+value+32 bytes).
//   - An encoder-stream parser that applies Insert With Name Ref,
//     Insert With Literal Name, Set Dynamic Table Capacity, and
//     Duplicate instructions.
//   - A Decoder type that holds the table and knows how to resolve
//     Required Insert Count and Base when decoding field sections.
//   - Decoder-stream output helpers (Section Acknowledgment, Insert
//     Count Increment, Stream Cancellation) so the server can keep
//     the peer's known-received count in sync.
//
// Out of scope in this file: the encoder side (our outgoing responses
// remain static-table-only; see Encode in qpack.go) and any asynchronous
// "blocked stream" machinery. Callers that advertise
// QPACK_BLOCKED_STREAMS=0 can only reference dynamic entries that have
// already been acknowledged, which keeps the decode path synchronous.

package qpack

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Errors specific to the dynamic-table path.
var (
	ErrBadDynamicIndex  = errors.New("qpack: dynamic-table index out of range")
	ErrCapacityExceeded = errors.New("qpack: dynamic-table capacity exceeded")
	ErrBlocked          = errors.New("qpack: required insert count exceeds known inserts (blocked)")
	ErrBadEncoderInstr  = errors.New("qpack: malformed encoder-stream instruction")
)

// entry is one row in the dynamic table.
type entry struct{ name, value string }

// qpackEntryOverhead is the fixed size component per entry per
// RFC 9204 §3.2.1: "The size of an entry is the sum of its name's
// length in bytes, its value's length in bytes, and 32."
const qpackEntryOverhead = 32

// DynamicTable is a ring buffer of (name, value) entries with an
// RFC 9204 size cap. Entries are indexed by absolute index, which
// increases monotonically with each insertion; the oldest entry
// currently resident is at absolute index (insertCount - len(entries)).
type DynamicTable struct {
	mu          sync.Mutex
	entries     []entry // oldest first
	byteSize    uint64  // current total size per §3.2.1
	capacity    uint64  // current capacity set by the peer
	maxCapacity uint64  // advertised ceiling (MUST NOT be exceeded)
	insertCount uint64  // total inserts since connection start

	// insertCh is a broadcast signal: closed whenever insertCount
	// advances, then replaced atomically under mu. Waiters in
	// WaitForInsert capture a reference under mu, release mu, then
	// block on the captured channel. Buffered(1) wouldn't work —
	// multiple request streams can be simultaneously blocked waiting
	// on the same dynamic-table insert, and a single-slot channel
	// only wakes one of them.
	insertCh chan struct{}
}

// NewDynamicTable returns a table capped at maxCapacity bytes. The
// peer may lower the effective capacity at runtime via a Set Dynamic
// Table Capacity instruction but never raise it above maxCapacity.
func NewDynamicTable(maxCapacity uint64) *DynamicTable {
	return &DynamicTable{maxCapacity: maxCapacity, insertCh: make(chan struct{})}
}

// notifyInsertLocked wakes every WaitForInsert goroutine. Caller holds
// t.mu.
func (t *DynamicTable) notifyInsertLocked() {
	close(t.insertCh)
	t.insertCh = make(chan struct{})
}

// WaitForInsert blocks until insertCount >= want, ctx is cancelled, or
// the (bounded) wait deadline fires. Returns nil when the count
// condition is satisfied, else context.Canceled / context.DeadlineExceeded.
//
// Used by the field-section decoder to honour a Required Insert Count
// that hasn't been reached yet — the peer is allowed to reference
// entries it has already transmitted on the encoder stream but whose
// Insert frames we're still processing, up to QPACK_BLOCKED_STREAMS.
func (t *DynamicTable) WaitForInsert(ctx context.Context, want uint64) error {
	for {
		t.mu.Lock()
		if t.insertCount >= want {
			t.mu.Unlock()
			return nil
		}
		ch := t.insertCh
		t.mu.Unlock()
		select {
		case <-ch:
			// Re-check loop.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// setCapacity handles an encoder-stream Set Dynamic Table Capacity
// instruction. Evicts oldest entries as needed to fit the new size.
func (t *DynamicTable) setCapacity(c uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c > t.maxCapacity {
		return fmt.Errorf("%w: set=%d max=%d", ErrCapacityExceeded, c, t.maxCapacity)
	}
	t.capacity = c
	t.evictLocked()
	return nil
}

// insert adds a new entry. Evicts oldest entries until the new entry
// fits, or returns ErrCapacityExceeded if even eviction of everything
// won't make room.
func (t *DynamicTable) insert(name, value string) error {
	size := uint64(len(name) + len(value) + qpackEntryOverhead)
	t.mu.Lock()
	defer t.mu.Unlock()
	if size > t.capacity {
		// RFC 9204 §3.2.2: an entry larger than the current capacity
		// is a decoder error; the encoder should never try this.
		return fmt.Errorf("%w: entry size %d > cap %d", ErrCapacityExceeded, size, t.capacity)
	}
	// Evict until the new entry fits.
	for t.byteSize+size > t.capacity && len(t.entries) > 0 {
		t.byteSize -= uint64(len(t.entries[0].name) + len(t.entries[0].value) + qpackEntryOverhead)
		t.entries = t.entries[1:]
	}
	t.entries = append(t.entries, entry{name: name, value: value})
	t.byteSize += size
	t.insertCount++
	t.notifyInsertLocked()
	return nil
}

// evictLocked drops oldest entries until byteSize <= capacity. Caller
// holds the mutex.
func (t *DynamicTable) evictLocked() {
	for t.byteSize > t.capacity && len(t.entries) > 0 {
		t.byteSize -= uint64(len(t.entries[0].name) + len(t.entries[0].value) + qpackEntryOverhead)
		t.entries = t.entries[1:]
	}
}

// getAbsolute returns the entry at the given absolute index, or
// ok=false if it has been evicted or doesn't yet exist.
func (t *DynamicTable) getAbsolute(abs uint64) (entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if abs >= t.insertCount {
		return entry{}, false
	}
	oldest := t.insertCount - uint64(len(t.entries))
	if abs < oldest {
		return entry{}, false
	}
	return t.entries[abs-oldest], true
}

// InsertCount returns the total number of inserts applied.
func (t *DynamicTable) InsertCount() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.insertCount
}

// duplicate copies an existing entry to the end of the table (its
// absolute index argument is relative to the current insert count
// per RFC 9204 §4.3.4 — "RelIndex" of 0 means the most recent entry).
func (t *DynamicTable) duplicate(relIndex uint64) error {
	t.mu.Lock()
	if uint64(len(t.entries)) <= relIndex {
		t.mu.Unlock()
		return fmt.Errorf("%w: duplicate rel=%d len=%d", ErrBadDynamicIndex, relIndex, len(t.entries))
	}
	srcIdx := len(t.entries) - 1 - int(relIndex)
	name := t.entries[srcIdx].name
	value := t.entries[srcIdx].value
	t.mu.Unlock()
	return t.insert(name, value)
}

// ---- Decoder ----------------------------------------------------------

// Decoder drives QPACK decoding for a single QUIC connection. It holds
// the peer's encoder's dynamic table (reconstructed from encoder-stream
// instructions) and tracks the insert count so field sections can be
// validated against their Required Insert Count.
//
// Decoder is safe to call from one goroutine at a time. The encoder-
// stream reader goroutine calls HandleEncoderStream; each request's
// goroutine calls DecodeFieldSection; a caller typically serializes
// those via an outer mutex or by routing everything through a single
// decoder-facing goroutine.
type Decoder struct {
	Table *DynamicTable
}

// NewDecoder returns a Decoder with a zero-sized table. The caller must
// set the maximum capacity (advertised in SETTINGS) before handing the
// Decoder any encoder-stream bytes.
func NewDecoder(maxCapacity uint64) *Decoder {
	return &Decoder{Table: NewDynamicTable(maxCapacity)}
}

// HandleEncoderStream consumes as much of buf as contains complete
// instructions and applies them to the table. Returns the tail that
// didn't form a complete instruction (the caller should prepend it to
// the next chunk) plus any error. A Set Dynamic Table Capacity that
// exceeds our advertised max, an Insert that references an
// out-of-range static/dynamic entry, or a malformed varint is fatal
// and aborts consumption at the failing instruction.
func (d *Decoder) HandleEncoderStream(buf []byte) ([]byte, error) {
	for len(buf) > 0 {
		b := buf[0]
		switch {
		case b&0b1000_0000 != 0:
			// Insert With Name Reference. T=b&0x40 (1=static, 0=dynamic).
			staticRef := b&0b0100_0000 != 0
			idx, n, err := readQPACKInt(buf, 6)
			if err != nil {
				return buf, nil // truncated — wait for more
			}
			if len(buf) <= n {
				return buf, nil
			}
			val, rest, err := readStringLiteral(buf[n:])
			if err != nil {
				if errors.Is(err, ErrTruncated) {
					return buf, nil
				}
				return buf, err
			}
			var name string
			if staticRef {
				if int(idx) >= len(StaticTable) {
					return buf, ErrBadRefIndex
				}
				name = StaticTable[idx].Name
			} else {
				// Dynamic: idx is relative to the latest insert (§3.2.6:
				// "relative indexing"). AbsIndex = insertCount - 1 - idx.
				ic := d.Table.InsertCount()
				if uint64(idx) >= ic {
					return buf, ErrBadDynamicIndex
				}
				e, ok := d.Table.getAbsolute(ic - 1 - uint64(idx))
				if !ok {
					return buf, ErrBadDynamicIndex
				}
				name = e.name
			}
			if err := d.Table.insert(name, val); err != nil {
				return buf, err
			}
			buf = rest
		case b&0b1100_0000 == 0b0100_0000:
			// Insert With Literal Name. H-bit = b&0x20; length 5-bit prefix.
			nameHuff := b&0b0010_0000 != 0
			nameLen, nAdv, err := readQPACKInt(buf, 5)
			if err != nil {
				return buf, nil
			}
			if uint64(len(buf)-nAdv) < nameLen {
				return buf, nil
			}
			rawName := buf[nAdv : nAdv+int(nameLen)]
			var name string
			if nameHuff {
				dst := make([]byte, 0, int(nameLen)*8/5+8)
				out, herr := huffmanDecode(dst, rawName)
				if herr != nil {
					return buf, herr
				}
				name = string(out)
			} else {
				name = string(rawName)
			}
			rest := buf[nAdv+int(nameLen):]
			val, rest2, err := readStringLiteral(rest)
			if err != nil {
				if errors.Is(err, ErrTruncated) {
					return buf, nil
				}
				return buf, err
			}
			if err := d.Table.insert(name, val); err != nil {
				return buf, err
			}
			buf = rest2
		case b&0b1110_0000 == 0b0010_0000:
			// Set Dynamic Table Capacity. 5-bit prefix.
			cap_, n, err := readQPACKInt(buf, 5)
			if err != nil {
				return buf, nil
			}
			if err := d.Table.setCapacity(cap_); err != nil {
				return buf, err
			}
			buf = buf[n:]
		case b&0b1110_0000 == 0b0000_0000:
			// Duplicate. 5-bit prefix; index is relative.
			idx, n, err := readQPACKInt(buf, 5)
			if err != nil {
				return buf, nil
			}
			if err := d.Table.duplicate(idx); err != nil {
				return buf, err
			}
			buf = buf[n:]
		default:
			return buf, ErrBadEncoderInstr
		}
	}
	return nil, nil
}

// DecodeFieldSection decodes a field section using the dynamic table.
// streamID is only used to bound Stream Cancellation semantics (not
// actually emitted here — the caller tracks stream completion and
// asks for a Section Acknowledgment via AckSection).
//
// If the section's Required Insert Count is greater than the current
// known insert count, ErrBlocked is returned. See DecodeFieldSectionCtx
// for the blocking variant that honours QPACK_BLOCKED_STREAMS>0.
//
// On success, the caller should emit a Section Acknowledgment on the
// decoder stream — but only if usedDynamic is true (RFC 9204 §4.4.1
// limits acks to sections that referenced the dynamic table).
func (d *Decoder) DecodeFieldSection(streamID uint64, block []byte) ([]Field, uint64, bool, error) {
	// Prefix: Encoded Required Insert Count (8-bit prefix), Base
	// (S + 7-bit DeltaBase).
	eric, n, err := readQPACKInt(block, 8)
	if err != nil {
		return nil, 0, false, err
	}
	block = block[n:]
	if len(block) == 0 {
		return nil, 0, false, ErrTruncated
	}
	sBit := block[0]&0x80 != 0
	deltaBase, n, err := readQPACKInt(block, 7)
	if err != nil {
		return nil, 0, false, err
	}
	block = block[n:]

	// Reconstruct Required Insert Count per RFC 9204 §4.5.1.1.
	insertCount := d.Table.InsertCount()
	maxEntries := d.Table.maxCapacity / 32
	var ric uint64
	if eric == 0 {
		ric = 0
	} else {
		if maxEntries == 0 {
			return nil, 0, false, fmt.Errorf("qpack: RIC non-zero but maxCapacity=0")
		}
		fullRange := 2 * maxEntries
		if eric > fullRange {
			return nil, 0, false, fmt.Errorf("qpack: EncodedRIC %d > 2*MaxEntries %d", eric, fullRange)
		}
		maxValue := insertCount + maxEntries
		maxWrapped := (maxValue / fullRange) * fullRange
		ric = maxWrapped + eric - 1
		if ric > maxValue {
			if ric < fullRange {
				return nil, 0, false, fmt.Errorf("qpack: RIC underflow")
			}
			ric -= fullRange
		}
	}
	if ric > insertCount {
		return nil, ric, false, ErrBlocked
	}

	// Reconstruct Base per §4.5.1.2.
	var base uint64
	if sBit {
		if deltaBase+1 > ric {
			return nil, ric, false, fmt.Errorf("qpack: base underflow (ric=%d delta=%d S=1)", ric, deltaBase)
		}
		base = ric - deltaBase - 1
	} else {
		base = ric + deltaBase
	}
	_ = streamID // placeholder for future stream-cancel tracking

	fields, b, used, err := decodeBody(block, d.Table, base)
	return fields, b, used, err
}

// defaultBlockedWait bounds how long a request will wait for an
// encoder-stream insert that its HEADERS frame references. 500 ms is
// long enough to absorb a real QPACK round-trip on a transatlantic
// link but short enough that a misbehaving encoder can't leak stuck
// request goroutines.
const defaultBlockedWait = 500

// DecodeFieldSectionCtx is the blocking variant of DecodeFieldSection.
// If the section's Required Insert Count exceeds the current known
// insert count, it waits (up to ctx's deadline, or a 500 ms internal
// cap) for encoder-stream inserts to arrive. This is the method to
// use when QPACK_BLOCKED_STREAMS > 0.
//
// Returns ErrBlocked only if the wait times out.
func (d *Decoder) DecodeFieldSectionCtx(ctx context.Context, streamID uint64, block []byte) ([]Field, uint64, bool, error) {
	// Parse the prefix without consuming the body, so we can decide
	// whether to wait before holding any per-stream resources.
	ric, body, err := d.peekRIC(block)
	if err != nil {
		return nil, 0, false, err
	}
	if ric > d.Table.InsertCount() {
		// Bound the wait. A misbehaving encoder must not strand a
		// request goroutine forever.
		wctx, cancel := boundedContext(ctx, defaultBlockedWait)
		defer cancel()
		if werr := d.Table.WaitForInsert(wctx, ric); werr != nil {
			return nil, ric, false, ErrBlocked
		}
	}
	_ = body
	return d.DecodeFieldSection(streamID, block)
}

// peekRIC parses just the field-section prefix (EncodedRIC + Base)
// and returns the reconstructed Required Insert Count plus the
// unread body. This lets DecodeFieldSectionCtx decide whether to
// wait without allocating fields on a doomed decode path.
func (d *Decoder) peekRIC(block []byte) (uint64, []byte, error) {
	orig := block
	eric, n, err := readQPACKInt(block, 8)
	if err != nil {
		return 0, orig, err
	}
	block = block[n:]
	if len(block) == 0 {
		return 0, orig, ErrTruncated
	}
	_, n, err = readQPACKInt(block[:], 7) // deltaBase — skipped
	if err != nil {
		return 0, orig, err
	}
	block = block[n:]

	insertCount := d.Table.InsertCount()
	maxEntries := d.Table.maxCapacity / 32
	if eric == 0 {
		return 0, block, nil
	}
	if maxEntries == 0 {
		return 0, block, fmt.Errorf("qpack: RIC non-zero but maxCapacity=0")
	}
	fullRange := 2 * maxEntries
	if eric > fullRange {
		return 0, block, fmt.Errorf("qpack: EncodedRIC %d > 2*MaxEntries %d", eric, fullRange)
	}
	maxValue := insertCount + maxEntries
	maxWrapped := (maxValue / fullRange) * fullRange
	ric := maxWrapped + eric - 1
	if ric > maxValue {
		if ric < fullRange {
			return 0, block, fmt.Errorf("qpack: RIC underflow")
		}
		ric -= fullRange
	}
	return ric, block, nil
}

// boundedContext returns a derived context whose deadline is the
// sooner of (parent deadline) and (now + capMillis).
func boundedContext(parent context.Context, capMillis int) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, time.Duration(capMillis)*time.Millisecond)
}

// decodeBody walks the field-section body, resolving static and
// dynamic references against table + base. usedDynamic is true if any
// decoded field came from the dynamic table (gates Section Ack).
func decodeBody(block []byte, table *DynamicTable, base uint64) ([]Field, uint64, bool, error) {
	var fields []Field
	var usedDynamic bool
	for len(block) > 0 {
		b := block[0]
		switch {
		case b&0b1000_0000 != 0:
			// Indexed Field Line. T=b&0x40.
			staticRef := b&0b0100_0000 != 0
			idx, n, err := readQPACKInt(block, 6)
			if err != nil {
				return nil, 0, false, err
			}
			block = block[n:]
			if staticRef {
				if int(idx) >= len(StaticTable) {
					return nil, 0, false, ErrBadRefIndex
				}
				e := StaticTable[idx]
				fields = append(fields, Field{Name: e.Name, Value: e.Value})
			} else {
				abs, ok := resolveDynamicRelative(base, idx)
				if !ok {
					return nil, 0, false, ErrBadDynamicIndex
				}
				e, ok := table.getAbsolute(abs)
				if !ok {
					return nil, 0, false, ErrBadDynamicIndex
				}
				usedDynamic = true
				fields = append(fields, Field{Name: e.name, Value: e.value})
			}
		case b&0b1100_0000 == 0b0100_0000:
			// Literal Field Line With Name Reference. N=b&0x20, T=b&0x10.
			staticRef := b&0b0001_0000 != 0
			idx, n, err := readQPACKInt(block, 4)
			if err != nil {
				return nil, 0, false, err
			}
			block = block[n:]
			val, rest, err := readStringLiteral(block)
			if err != nil {
				return nil, 0, false, err
			}
			block = rest
			if staticRef {
				if int(idx) >= len(StaticTable) {
					return nil, 0, false, ErrBadRefIndex
				}
				fields = append(fields, Field{Name: StaticTable[idx].Name, Value: val})
			} else {
				abs, ok := resolveDynamicRelative(base, idx)
				if !ok {
					return nil, 0, false, ErrBadDynamicIndex
				}
				e, ok := table.getAbsolute(abs)
				if !ok {
					return nil, 0, false, ErrBadDynamicIndex
				}
				usedDynamic = true
				fields = append(fields, Field{Name: e.name, Value: val})
			}
		case b&0b1110_0000 == 0b0010_0000:
			// Literal Field Line With Literal Name (N=b&0x10, H=b&0x08).
			nameHuff := b&0b0000_1000 != 0
			nameLen, nAdv, err := readQPACKInt(block, 3)
			if err != nil {
				return nil, 0, false, err
			}
			block = block[nAdv:]
			if uint64(len(block)) < nameLen {
				return nil, 0, false, ErrTruncated
			}
			rawName := block[:nameLen]
			block = block[nameLen:]
			var name string
			if nameHuff {
				dst := make([]byte, 0, int(nameLen)*8/5+8)
				out, err := huffmanDecode(dst, rawName)
				if err != nil {
					return nil, 0, false, err
				}
				name = string(out)
			} else {
				name = string(rawName)
			}
			val, rest, err := readStringLiteral(block)
			if err != nil {
				return nil, 0, false, err
			}
			fields = append(fields, Field{Name: name, Value: val})
			block = rest
		case b&0b1111_0000 == 0b0001_0000:
			// Indexed Field Line With Post-Base Index. 4-bit prefix.
			idx, n, err := readQPACKInt(block, 4)
			if err != nil {
				return nil, 0, false, err
			}
			block = block[n:]
			abs := base + idx
			e, ok := table.getAbsolute(abs)
			if !ok {
				return nil, 0, false, ErrBadDynamicIndex
			}
			usedDynamic = true
			fields = append(fields, Field{Name: e.name, Value: e.value})
		case b&0b1111_0000 == 0b0000_0000:
			// Literal Field Line With Post-Base Name Reference. 3-bit prefix.
			idx, n, err := readQPACKInt(block, 3)
			if err != nil {
				return nil, 0, false, err
			}
			block = block[n:]
			val, rest, err := readStringLiteral(block)
			if err != nil {
				return nil, 0, false, err
			}
			block = rest
			abs := base + idx
			e, ok := table.getAbsolute(abs)
			if !ok {
				return nil, 0, false, ErrBadDynamicIndex
			}
			usedDynamic = true
			fields = append(fields, Field{Name: e.name, Value: val})
		default:
			return nil, 0, false, fmt.Errorf("qpack: unsupported representation 0x%02x", b)
		}
	}
	return fields, base, usedDynamic, nil
}

// resolveDynamicRelative maps a pre-base relative index (per §4.5.2)
// to an absolute table index using Base. Returns ok=false on underflow.
func resolveDynamicRelative(base, relIdx uint64) (uint64, bool) {
	if relIdx+1 > base {
		return 0, false
	}
	return base - 1 - relIdx, true
}

// ---- Decoder-stream output --------------------------------------------

// EncodeSectionAck emits a Section Acknowledgment for the given stream
// ID (RFC 9204 §4.4.1): pattern 1xxxxxxx, 7-bit prefix.
func EncodeSectionAck(dst []byte, streamID uint64) []byte {
	return appendQPACKInt(dst, 0x80, 7, streamID)
}

// EncodeStreamCancel emits a Stream Cancellation (§4.4.2): pattern
// 01xxxxxx, 6-bit prefix.
func EncodeStreamCancel(dst []byte, streamID uint64) []byte {
	return appendQPACKInt(dst, 0x40, 6, streamID)
}

// EncodeInsertCountIncrement emits an Insert Count Increment
// (§4.4.3): pattern 00xxxxxx, 6-bit prefix. increment is the number
// of new inserts since the last increment we sent.
func EncodeInsertCountIncrement(dst []byte, increment uint64) []byte {
	return appendQPACKInt(dst, 0x00, 6, increment)
}
