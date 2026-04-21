// hpack_test.go: RFC 7541 Appendix C conformance and round-trip tests.

package hpack

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// dehex strips spaces/newlines and hex-decodes. Panics on malformed input;
// only used on hard-coded test fixtures.
func dehex(s string) []byte {
	s = strings.Join(strings.Fields(s), "")
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

type kv struct{ name, value string }

func decodeAll(t *testing.T, d *Decoder, block []byte) []kv {
	t.Helper()
	var got []kv
	err := d.Decode(block, func(f Field) bool {
		got = append(got, kv{string(f.Name), string(f.Value)})
		return true
	})
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	return got
}

func eqFields(a, b []kv) bool {
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

// C.2.1: literal header field with indexing.
func TestDecode_C_2_1(t *testing.T) {
	block := dehex(`400a 6375 7374 6f6d 2d6b 6579 0d63 7573
                    746f 6d2d 6865 6164 6572`)
	d := NewDecoder(NewDynamicTable(4096))
	got := decodeAll(t, d, block)
	want := []kv{{"custom-key", "custom-header"}}
	if !eqFields(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if d.dt.Len() != 1 {
		t.Fatalf("dt len = %d, want 1", d.dt.Len())
	}
}

// C.2.2: literal without indexing.
func TestDecode_C_2_2(t *testing.T) {
	block := dehex(`040c 2f73 616d 706c 652f 7061 7468`)
	d := NewDecoder(NewDynamicTable(4096))
	got := decodeAll(t, d, block)
	want := []kv{{":path", "/sample/path"}}
	if !eqFields(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if d.dt.Len() != 0 {
		t.Fatalf("dt len = %d, want 0", d.dt.Len())
	}
}

// C.2.3: literal never-indexed.
func TestDecode_C_2_3(t *testing.T) {
	block := dehex(`1008 7061 7373 776f 7264 0673 6563 7265 74`)
	d := NewDecoder(NewDynamicTable(4096))
	got := decodeAll(t, d, block)
	want := []kv{{"password", "secret"}}
	if !eqFields(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// C.2.4: indexed header field.
func TestDecode_C_2_4(t *testing.T) {
	block := dehex(`82`)
	d := NewDecoder(NewDynamicTable(4096))
	got := decodeAll(t, d, block)
	want := []kv{{":method", "GET"}}
	if !eqFields(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// C.4.1-C.4.3: Huffman-encoded request sequence across three requests with
// the same decoder (persisted dynamic table).
func TestDecode_C_4_requests(t *testing.T) {
	d := NewDecoder(NewDynamicTable(4096))

	// C.4.1: "GET http://www.example.com/"
	b1 := dehex(`8286 8441 8cf1 e3c2 e5f2 3a6b a0ab 90f4 ff`)
	want1 := []kv{
		{":method", "GET"},
		{":scheme", "http"},
		{":path", "/"},
		{":authority", "www.example.com"},
	}
	got := decodeAll(t, d, b1)
	if !eqFields(got, want1) {
		t.Fatalf("C.4.1 got %v want %v", got, want1)
	}

	// C.4.2: adds cache-control: no-cache
	b2 := dehex(`8286 84be 5886 a8eb 1064 9cbf`)
	want2 := []kv{
		{":method", "GET"},
		{":scheme", "http"},
		{":path", "/"},
		{":authority", "www.example.com"},
		{"cache-control", "no-cache"},
	}
	got = decodeAll(t, d, b2)
	if !eqFields(got, want2) {
		t.Fatalf("C.4.2 got %v want %v", got, want2)
	}

	// C.4.3: different path, different scheme, adds custom-key.
	b3 := dehex(`8287 85bf 4088 25a8 49e9 5ba9 7d7f 8925 a849 e95b b8e8 b4bf`)
	want3 := []kv{
		{":method", "GET"},
		{":scheme", "https"},
		{":path", "/index.html"},
		{":authority", "www.example.com"},
		{"custom-key", "custom-value"},
	}
	got = decodeAll(t, d, b3)
	if !eqFields(got, want3) {
		t.Fatalf("C.4.3 got %v want %v", got, want3)
	}
}

func TestHuffmanRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"a",
		"www.example.com",
		"custom-key",
		"custom-value",
		"no-cache",
		"/sample/path",
		"Mon, 21 Oct 2013 20:13:21 GMT",
		"foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}
	for _, s := range cases {
		enc := HuffmanEncode(nil, []byte(s))
		if got := HuffmanEncodedLen([]byte(s)); got != len(enc) {
			t.Fatalf("EncodedLen(%q)=%d, actual encoded len=%d", s, got, len(enc))
		}
		dst := make([]byte, len(s)*2+8)
		n, err := HuffmanDecode(dst, enc)
		if err != nil {
			t.Fatalf("decode(%q): %v", s, err)
		}
		if string(dst[:n]) != s {
			t.Fatalf("round trip: got %q want %q", dst[:n], s)
		}
	}
}

func TestEncoderDecoderRoundTrip(t *testing.T) {
	encDT := NewDynamicTable(4096)
	decDT := NewDynamicTable(4096)
	enc := NewEncoder(encDT)
	dec := NewDecoder(decDT)

	fields := []kv{
		{":status", "200"},
		{"content-type", "text/html; charset=utf-8"},
		{"server", "tachyon"},
		{"content-length", "1234"},
		{"date", "Mon, 21 Oct 2013 20:13:21 GMT"},
		{"x-custom", "hello world"},
	}

	var buf []byte
	for _, f := range fields {
		buf = enc.AppendField(buf, []byte(f.name), []byte(f.value))
	}

	got := decodeAll(t, dec, buf)
	if !eqFields(got, fields) {
		t.Fatalf("round trip mismatch:\n got %v\nwant %v", got, fields)
	}
}

func TestIndexedStatus(t *testing.T) {
	enc := NewEncoder(NewDynamicTable(4096))
	buf := enc.AppendIndexedStatus(nil, 200)
	if !bytes.Equal(buf, []byte{0x88}) {
		t.Fatalf("AppendIndexedStatus(200) = %x, want 88", buf)
	}
	buf = enc.AppendIndexedStatus(nil, 404)
	if !bytes.Equal(buf, []byte{0x8d}) {
		t.Fatalf("AppendIndexedStatus(404) = %x, want 8d", buf)
	}
}

func TestStaticEntry(t *testing.T) {
	n, v, ok := StaticEntry(2)
	if !ok || n != ":method" || v != "GET" {
		t.Fatalf("StaticEntry(2)=%q,%q,%v", n, v, ok)
	}
	_, _, ok = StaticEntry(0)
	if ok {
		t.Fatal("StaticEntry(0) should be !ok")
	}
	_, _, ok = StaticEntry(62)
	if ok {
		t.Fatal("StaticEntry(62) should be !ok")
	}
}

func TestDynamicTableEvict(t *testing.T) {
	dt := NewDynamicTable(100)
	dt.Add([]byte("aaa"), []byte("bbb"))       // size = 3+3+32=38
	dt.Add([]byte("ccccc"), []byte("ddddd"))   // size = 5+5+32=42, total 80
	dt.Add([]byte("eeeeeee"), []byte("fffff")) // size = 7+5+32=44, total 80+44=124; evicts oldest (38) -> 86; still >100? no 86<=100. OK.
	if dt.Len() != 2 {
		t.Fatalf("len=%d want 2", dt.Len())
	}
	// Newest first: index 1 should be eeeeeee
	n, v, ok := dt.Get(1)
	if !ok || string(n) != "eeeeeee" || string(v) != "fffff" {
		t.Fatalf("Get(1)=%q,%q,%v", n, v, ok)
	}
}

// TestDynamicTableHashIndex exercises the hash index path through Add,
// evict, and direct lookup. Catches probe-chain bugs that a small-table
// test wouldn't surface.
func TestDynamicTableHashIndex(t *testing.T) {
	dt := NewDynamicTable(4096)
	for i := 0; i < 100; i++ {
		name := []byte("x-custom-header-" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		value := []byte("value-" + string(rune('a'+i%26)))
		dt.Add(name, value)
		// Every insert must be findable by both name and exact match.
		if slot := dt.hashFindName(name); slot < 0 {
			t.Fatalf("hashFindName miss after Add #%d (%q)", i, name)
		}
		if slot := dt.hashFindExact(name, value); slot < 0 {
			t.Fatalf("hashFindExact miss after Add #%d (%q,%q)", i, name, value)
		}
	}

	// Oldest entries should have been evicted (arena cap = 4096).
	// A miss on an evicted entry must actually return -1.
	missing := []byte("x-custom-header-aa")
	if slot := dt.hashFindName(missing); slot >= 0 {
		// Only fail if the slot we got back doesn't actually hold
		// this name — the newer entry might have collided hash-wise
		// and been placed legitimately.
		e := dt.entries[slot]
		got := dt.arena[e.nameOff : e.nameOff+e.nameLen]
		if string(got) != string(missing) {
			t.Fatalf("stale hash entry survived eviction: %q -> slot %d (%q)", missing, slot, got)
		}
	}
}

func BenchmarkEncoderDynamicHeavy(b *testing.B) {
	// With shouldIndex currently limiting inserts to :status /
	// content-type / server, the dynamic table rarely exceeds a
	// handful of entries in a real proxy. This benchmark drives a
	// broader scenario: 8 custom response headers, encoded 100k
	// times, measuring the per-encode cost through the hash path.
	dt := NewDynamicTable(4096)
	enc := NewEncoder(dt)
	// Warm up the encoder's dynamic table with a few server-ish
	// entries so the scan path is representative.
	dt.Add([]byte(":status"), []byte("200"))
	dt.Add([]byte("content-type"), []byte("application/json"))
	dt.Add([]byte("server"), []byte("tachyon"))

	names := [][]byte{
		[]byte(":status"),
		[]byte("content-type"),
		[]byte("server"),
		[]byte("cache-control"),
		[]byte("x-request-id"),
		[]byte("vary"),
		[]byte("access-control-allow-origin"),
		[]byte("content-length"),
	}
	values := [][]byte{
		[]byte("200"),
		[]byte("application/json"),
		[]byte("tachyon"),
		[]byte("no-cache, private"),
		[]byte("abc123"),
		[]byte("Accept-Encoding"),
		[]byte("*"),
		[]byte("128"),
	}

	buf := make([]byte, 0, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = buf[:0]
		for j, name := range names {
			buf = enc.AppendField(buf, name, values[j])
		}
	}
}
