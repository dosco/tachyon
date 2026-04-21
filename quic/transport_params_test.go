// transport_params_test.go: RFC 9000 §18.2 transport-parameter
// encoding / parsing coverage. Backstops the bug where the server's
// initial_source_connection_id was set to the client's DCID instead of
// the server's chosen SCID, which strict clients (ngtcp2) reject with
// TRANSPORT_PARAMETER_ERROR.

package quic

import (
	"bytes"
	"testing"

	"tachyon/quic/packet"
)

// walkParams decodes a TP extension into an ordered (id, value) list.
// Keeps the whole blob intact so we can assert on exact bytes of the
// connection-id params, which parsePeerTransportParams intentionally
// ignores.
func walkParams(t *testing.T, b []byte) []struct {
	id  uint64
	val []byte
} {
	t.Helper()
	var out []struct {
		id  uint64
		val []byte
	}
	for len(b) > 0 {
		id, n, ok := packet.ReadVarint(b)
		if !ok {
			t.Fatalf("tp id truncated")
		}
		b = b[n:]
		ln, n, ok := packet.ReadVarint(b)
		if !ok {
			t.Fatalf("tp len truncated")
		}
		b = b[n:]
		if uint64(len(b)) < ln {
			t.Fatalf("tp value truncated")
		}
		out = append(out, struct {
			id  uint64
			val []byte
		}{id, append([]byte(nil), b[:ln]...)})
		b = b[ln:]
	}
	return out
}

func findParam(ps []struct {
	id  uint64
	val []byte
}, id uint64) ([]byte, bool) {
	for _, p := range ps {
		if p.id == id {
			return p.val, true
		}
	}
	return nil, false
}

// TestEncodeServerTransportParams_CIDs is the direct regression test
// for the ngtcp2-client interop bug: original_destination_connection_id
// (0x00) must equal the client's first-Initial DCID, and
// initial_source_connection_id (0x0f) must equal the SCID the server
// puts in its own Initial packets.
func TestEncodeServerTransportParams_CIDs(t *testing.T) {
	origDCID := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11}
	localSCID := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

	blob := encodeServerTransportParams(origDCID, localSCID)
	params := walkParams(t, blob)

	got, ok := findParam(params, 0x00)
	if !ok {
		t.Fatal("missing original_destination_connection_id (0x00)")
	}
	if !bytes.Equal(got, origDCID) {
		t.Fatalf("original_destination_connection_id = %x, want %x", got, origDCID)
	}

	got, ok = findParam(params, 0x0f)
	if !ok {
		t.Fatal("missing initial_source_connection_id (0x0f)")
	}
	if !bytes.Equal(got, localSCID) {
		t.Fatalf("initial_source_connection_id = %x, want %x (regression: was set to origDCID)", got, localSCID)
	}

	// Flow-control / streams params survive through the shared parser.
	tp, err := parsePeerTransportParams(blob)
	if err != nil {
		t.Fatalf("parsePeerTransportParams: %v", err)
	}
	if tp.InitialMaxData == 0 {
		t.Fatal("initial_max_data not round-tripped")
	}
	if tp.InitialMaxStreamsBidi == 0 {
		t.Fatal("initial_max_streams_bidi not round-tripped")
	}
	if tp.MaxIdleTimeoutMS == 0 {
		t.Fatal("max_idle_timeout not round-tripped")
	}
}

// TestEncodeServerTransportParams_CIDsDistinct guards against a future
// refactor re-introducing the "pass origDCID for both" bug.
func TestEncodeServerTransportParams_CIDsDistinct(t *testing.T) {
	origDCID := []byte{1, 2, 3, 4}
	localSCID := []byte{9, 9, 9, 9}

	blob := encodeServerTransportParams(origDCID, localSCID)
	params := walkParams(t, blob)
	odcid, _ := findParam(params, 0x00)
	iscid, _ := findParam(params, 0x0f)
	if bytes.Equal(odcid, iscid) {
		t.Fatalf("original_destination_connection_id == initial_source_connection_id (%x); these must come from different sources", odcid)
	}
}
