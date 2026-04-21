package qpack

import (
	"errors"
	"testing"
)

// TestDynamicTable_InsertAndEviction covers the size-driven eviction
// logic in the ring buffer: insert entries until capacity is exceeded
// and confirm the oldest entries fall off and the size accounting
// tracks the §3.2.1 formula (name+value+32).
func TestDynamicTable_InsertAndEviction(t *testing.T) {
	tbl := NewDynamicTable(4096)
	if err := tbl.setCapacity(128); err != nil {
		t.Fatalf("setCapacity: %v", err)
	}
	// Each entry: 1+1+32 = 34 bytes. 128/34 = 3 entries fit.
	for _, v := range []string{"a", "b", "c", "d"} {
		if err := tbl.insert("x", v); err != nil {
			t.Fatalf("insert %q: %v", v, err)
		}
	}
	if ic := tbl.InsertCount(); ic != 4 {
		t.Fatalf("InsertCount=%d want 4", ic)
	}
	// With cap=128 and 4 inserts of 34 bytes each, only the last 3 fit
	// (3*34=102 <= 128 < 4*34=136). Oldest (a) should be evicted.
	if _, ok := tbl.getAbsolute(0); ok {
		t.Errorf("abs=0 (first entry) should have been evicted")
	}
	for abs := uint64(1); abs < 4; abs++ {
		if _, ok := tbl.getAbsolute(abs); !ok {
			t.Errorf("abs=%d missing", abs)
		}
	}
}

// TestDynamicTable_CapacityCeiling confirms the decoder won't accept a
// capacity setting above the advertised max.
func TestDynamicTable_CapacityCeiling(t *testing.T) {
	tbl := NewDynamicTable(1024)
	if err := tbl.setCapacity(2048); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("setCapacity(2048): got %v want ErrCapacityExceeded", err)
	}
}

// TestEncoderStream_InsertWithLiteralName exercises the 01xxxxxx path:
// an insert instruction with both name and value as literal strings.
// Verifies size accounting and insert-count progression.
func TestEncoderStream_InsertWithLiteralName(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)

	// Build: 01 0 0 nameLen "x-debug" then 0 valLen "yes".
	var buf []byte
	// 01 H=0 prefix=5, name-len=7 ("x-debug")
	buf = appendQPACKInt(buf, 0x40, 5, 7)
	buf = append(buf, "x-debug"...)
	// value: 7-bit prefix literal, H=0, len=3
	buf = appendQPACKInt(buf, 0x00, 7, 3)
	buf = append(buf, "yes"...)

	tail, err := d.HandleEncoderStream(buf)
	if err != nil {
		t.Fatalf("HandleEncoderStream: %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("unexpected tail: %x", tail)
	}
	if ic := d.Table.InsertCount(); ic != 1 {
		t.Fatalf("InsertCount=%d want 1", ic)
	}
	e, ok := d.Table.getAbsolute(0)
	if !ok {
		t.Fatal("entry 0 not present")
	}
	if e.name != "x-debug" || e.value != "yes" {
		t.Fatalf("entry = (%q, %q) want (x-debug, yes)", e.name, e.value)
	}
}

// TestEncoderStream_SetCapacity hits the 001xxxxx opcode.
func TestEncoderStream_SetCapacity(t *testing.T) {
	d := NewDecoder(4096)
	// Set Dynamic Table Capacity = 256. 001xxxxx with 5-bit prefix.
	buf := appendQPACKInt(nil, 0x20, 5, 256)
	tail, err := d.HandleEncoderStream(buf)
	if err != nil {
		t.Fatalf("HandleEncoderStream: %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("tail: %x", tail)
	}
	if d.Table.capacity != 256 {
		t.Fatalf("capacity=%d want 256", d.Table.capacity)
	}
}

// TestEncoderStream_InsertWithNameRef_Static covers the name-ref path
// pointing into the static table. RFC 9204 Appendix A: static entry 0
// is :authority.
func TestEncoderStream_InsertWithNameRef_Static(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)
	// 1TTTxxxx, T=1 (static), 6-bit prefix. idx=0 (:authority).
	var buf []byte
	buf = appendQPACKInt(buf, 0xC0, 6, 0)
	buf = appendQPACKInt(buf, 0x00, 7, 9)
	buf = append(buf, "example.c"...)
	tail, err := d.HandleEncoderStream(buf)
	if err != nil {
		t.Fatalf("HandleEncoderStream: %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("tail: %x", tail)
	}
	e, ok := d.Table.getAbsolute(0)
	if !ok {
		t.Fatal("entry 0 not present")
	}
	if e.name != ":authority" || e.value != "example.c" {
		t.Fatalf("entry = (%q, %q)", e.name, e.value)
	}
}

// TestDecodeFieldSection_DynamicIndexed decodes a field section that
// references an entry already inserted via the encoder stream.
func TestDecodeFieldSection_DynamicIndexed(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)
	_ = d.Table.insert(":path", "/api/v1/x")
	_ = d.Table.insert("x-api-key", "secret")

	// Build a field section:
	//   RIC=2 → encoded RIC = (2 mod 2*MaxEntries) + 1 = 3
	//   Base=2 (S=0, DeltaBase=0)
	//   body: Indexed (dynamic) relIdx=0 → :path /api/v1/x
	//         Indexed (dynamic) relIdx=1 → x-api-key secret
	var block []byte
	block = appendQPACKInt(block, 0x00, 8, 3) // eRIC=3
	block = appendQPACKInt(block, 0x00, 7, 0) // S=0 DeltaBase=0 → Base=RIC=2
	// Indexed (dynamic) relIdx=0: pattern 1TTTxxxx with T=0, 6-bit prefix.
	block = appendQPACKInt(block, 0x80, 6, 0)
	// Indexed (dynamic) relIdx=1.
	block = appendQPACKInt(block, 0x80, 6, 1)

	fields, _, _, err := d.DecodeFieldSection(42, block)
	if err != nil {
		t.Fatalf("DecodeFieldSection: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("len(fields)=%d want 2", len(fields))
	}
	// Base=2, relIdx=0 → absolute = 2-1-0 = 1 → x-api-key/secret.
	// Base=2, relIdx=1 → absolute = 2-1-1 = 0 → :path /api/v1/x.
	if fields[0].Name != "x-api-key" || fields[0].Value != "secret" {
		t.Errorf("fields[0]=%+v", fields[0])
	}
	if fields[1].Name != ":path" || fields[1].Value != "/api/v1/x" {
		t.Errorf("fields[1]=%+v", fields[1])
	}
}

// TestDecodeFieldSection_Blocked verifies RIC > insertCount returns
// ErrBlocked (since we advertise blocked_streams=0).
func TestDecodeFieldSection_Blocked(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)
	// Table empty. Request RIC=1 (encoded as 2).
	var block []byte
	block = appendQPACKInt(block, 0x00, 8, 2) // eRIC=2 → RIC=1
	block = appendQPACKInt(block, 0x00, 7, 0)

	_, _, _, err := d.DecodeFieldSection(1, block)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("got %v want ErrBlocked", err)
	}
}

// TestDecodeFieldSection_PostBase exercises the 0001xxxx post-base
// indexed form: the request references entries inserted *after* Base.
func TestDecodeFieldSection_PostBase(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)
	_ = d.Table.insert("server", "tachyon")

	// RIC=1 (encoded 2), Base=0 (S=1, DeltaBase=0 → Base=RIC-0-1=0).
	// Post-Base index 0 → absolute = Base+0 = 0 → "server: tachyon".
	var block []byte
	block = appendQPACKInt(block, 0x00, 8, 2)
	block = appendQPACKInt(block, 0x80, 7, 0) // S=1, DeltaBase=0
	// Post-Base Indexed pattern 0001xxxx, 4-bit prefix.
	block = appendQPACKInt(block, 0x10, 4, 0)

	fields, _, _, err := d.DecodeFieldSection(3, block)
	if err != nil {
		t.Fatalf("DecodeFieldSection: %v", err)
	}
	if len(fields) != 1 || fields[0].Name != "server" || fields[0].Value != "tachyon" {
		t.Fatalf("fields=%+v", fields)
	}
}

// TestDecoderStreamOutput pins the wire format of the three decoder-
// stream instructions.
func TestDecoderStreamOutput(t *testing.T) {
	// Section Ack: 1xxxxxxx, 7-bit prefix. streamID=5 → 0x85.
	if got := EncodeSectionAck(nil, 5); len(got) != 1 || got[0] != 0x85 {
		t.Errorf("SectionAck(5)=%x want 85", got)
	}
	// Stream Cancel: 01xxxxxx, 6-bit prefix. streamID=3 → 0x43.
	if got := EncodeStreamCancel(nil, 3); len(got) != 1 || got[0] != 0x43 {
		t.Errorf("StreamCancel(3)=%x want 43", got)
	}
	// Insert Count Increment: 00xxxxxx, 6-bit prefix. inc=10 → 0x0A.
	if got := EncodeInsertCountIncrement(nil, 10); len(got) != 1 || got[0] != 0x0A {
		t.Errorf("InsertCountIncrement(10)=%x want 0A", got)
	}
}

// TestEncoderStream_Duplicate exercises the 000xxxxx opcode.
func TestEncoderStream_Duplicate(t *testing.T) {
	d := NewDecoder(4096)
	_ = d.Table.setCapacity(512)
	_ = d.Table.insert("k", "v1")
	_ = d.Table.insert("k", "v2")

	// Duplicate rel=1 → copies entry at (len-1-1)=0 → (k, v1).
	buf := appendQPACKInt(nil, 0x00, 5, 1)
	tail, err := d.HandleEncoderStream(buf)
	if err != nil {
		t.Fatalf("HandleEncoderStream: %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("tail: %x", tail)
	}
	if ic := d.Table.InsertCount(); ic != 3 {
		t.Fatalf("InsertCount=%d want 3", ic)
	}
	e, _ := d.Table.getAbsolute(2)
	if e.name != "k" || e.value != "v1" {
		t.Fatalf("dup entry = (%q, %q) want (k, v1)", e.name, e.value)
	}
}
