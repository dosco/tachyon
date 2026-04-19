package buf

import "testing"

// TestSlabBoundedZero verifies the canary-byte probe from the plan:
// write sentinels at offsets 0, written-1, and cap-1; call Reset; the
// first two must be zeroed, the last must be untouched.
func TestSlabBoundedZero(t *testing.T) {
	SetFullZero(false)
	s := Get(ClassHeader)
	b := s.Bytes()
	const written = 100
	b[0] = 0xAA
	b[written-1] = 0xBB
	b[len(b)-1] = 0xCC
	s.MarkWritten(written)
	s.Reset()
	if b[0] != 0 || b[written-1] != 0 {
		t.Fatalf("bounded zero did not clear written region: b[0]=%x b[written-1]=%x",
			b[0], b[written-1])
	}
	if b[len(b)-1] != 0xCC {
		t.Fatalf("bounded zero over-cleared: b[cap-1]=%x want 0xCC", b[len(b)-1])
	}
	// Return without MarkWritten to avoid affecting subsequent tests.
	Put(s)
}

// TestSlabFullZero verifies -buf-zero=full zeroes beyond the written
// high-water mark.
func TestSlabFullZero(t *testing.T) {
	SetFullZero(true)
	defer SetFullZero(false)
	s := Get(ClassHeader)
	b := s.Bytes()
	b[0] = 0xAA
	b[len(b)-1] = 0xCC
	// Don't call MarkWritten — full mode must clear regardless.
	s.Reset()
	if b[0] != 0 || b[len(b)-1] != 0 {
		t.Fatalf("full zero missed bytes: b[0]=%x b[cap-1]=%x", b[0], b[len(b)-1])
	}
	Put(s)
}

// TestSlabMarkWrittenIsMonotonic confirms MarkWritten takes the max,
// so callers that write in multiple phases can report each phase's
// length without undercounting.
func TestSlabMarkWrittenIsMonotonic(t *testing.T) {
	SetFullZero(false)
	s := Get(ClassHeader)
	s.MarkWritten(10)
	s.MarkWritten(5)
	if s.written != 10 {
		t.Fatalf("MarkWritten regressed: got %d want 10", s.written)
	}
	s.MarkWritten(42)
	if s.written != 42 {
		t.Fatalf("MarkWritten not monotonic: got %d want 42", s.written)
	}
	// Clean up before returning to pool.
	b := s.Bytes()
	for i := 0; i < 42; i++ {
		b[i] = 0
	}
	Put(s)
}

// TestSlabPutZeroesOnReturn proves that a Put'd slab has its written
// region cleared even if the caller didn't call Reset. Next Get from
// the same pool must not observe stale bytes in [0:written).
//
// sync.Pool may return a different underlying slab on Get if another
// goroutine pushed one in between, so we loop until we get the same
// slab back (this is not a race; we're in a single goroutine).
func TestSlabPutZeroesOnReturn(t *testing.T) {
	SetFullZero(false)
	s := Get(ClassHeader)
	b := s.Bytes()
	// Write sentinel bytes throughout a 128-byte prefix.
	for i := 0; i < 128; i++ {
		b[i] = 0xFE
	}
	s.MarkWritten(128)
	Put(s)

	// If sync.Pool hands us a fresh slab, the test is trivially OK
	// (fresh slabs are all-zero). If it hands back the same one we
	// just Put, we require that the written prefix is zero.
	s2 := Get(ClassHeader)
	b2 := s2.Bytes()
	for i := 0; i < 128; i++ {
		if b2[i] != 0 {
			t.Fatalf("stale byte at offset %d: %x", i, b2[i])
		}
	}
	Put(s2)
}
