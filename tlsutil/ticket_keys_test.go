package tlsutil

import (
	"bytes"
	"testing"
)

// TestDeriveTicketKeysDeterministic is the core property: two workers
// (or two calls in the same process) sharing a seed + epoch get byte-
// identical ticket-key material. Without this, SO_REUSEPORT siblings
// can't resume each other's tickets.
func TestDeriveTicketKeysDeterministic(t *testing.T) {
	seed := bytes.Repeat([]byte{0x42}, 32)
	a := DeriveTicketKeysAt(seed, 1234)
	b := DeriveTicketKeysAt(seed, 1234)
	if len(a) != len(b) {
		t.Fatalf("length mismatch: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("slot %d differs: %x vs %x", i, a[i], b[i])
		}
	}
}

// TestDeriveTicketKeysDistinctEpochs verifies successive epochs produce
// distinct keys (otherwise rotation would be a no-op) and that the
// current epoch's key overlaps with the next epoch's fallback set (so
// tickets issued near the boundary still decrypt).
func TestDeriveTicketKeysDistinctEpochs(t *testing.T) {
	seed := bytes.Repeat([]byte{0xAA}, 32)
	e0 := DeriveTicketKeysAt(seed, 100)
	e1 := DeriveTicketKeysAt(seed, 101)

	if e0[0] == e1[0] {
		t.Fatalf("epoch primary keys are identical across epochs — rotation is broken")
	}
	// e0 primary must live on as e1's first fallback: a ticket issued
	// just before the rotation must still decrypt right after.
	if e0[0] != e1[1] {
		t.Errorf("epoch-100 primary not found as epoch-101 fallback: %x vs %x", e0[0], e1[1])
	}
}

// TestDeriveTicketKeysDifferentSeeds guards against trivial mistakes
// in the HKDF wiring — two distinct seeds must yield distinct keys.
func TestDeriveTicketKeysDifferentSeeds(t *testing.T) {
	s1 := bytes.Repeat([]byte{0x01}, 32)
	s2 := bytes.Repeat([]byte{0x02}, 32)
	k1 := DeriveTicketKeysAt(s1, 7)
	k2 := DeriveTicketKeysAt(s2, 7)
	if k1[0] == k2[0] {
		t.Fatalf("distinct seeds produced identical ticket keys")
	}
}
