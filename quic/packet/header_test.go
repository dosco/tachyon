package packet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// buildInitial constructs a minimal valid long-header Initial packet
// (still protected). Payload is arbitrary since tests only exercise
// header parsing.
func buildInitial(version uint32, dcid, scid, token, payload []byte) []byte {
	var b []byte
	// First byte: long form (0x80) + fixed bit (0x40) + type=Initial (0<<4)
	// + reserved (0) + pn_len-1=0. Low 4 bits are protected in real life.
	b = append(b, 0xc0)
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v, version)
	b = append(b, v...)
	b = append(b, byte(len(dcid)))
	b = append(b, dcid...)
	b = append(b, byte(len(scid)))
	b = append(b, scid...)
	b = AppendVarint(b, uint64(len(token)))
	b = append(b, token...)
	// Length is a placeholder varint covering packet-number + payload.
	b = AppendVarint(b, uint64(len(payload)))
	b = append(b, payload...)
	return b
}

func TestParseInitial(t *testing.T) {
	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	scid := []byte{9, 10, 11, 12}
	token := []byte("hello-token")
	payload := bytes.Repeat([]byte{0xaa}, 32)
	pkt := buildInitial(Version1, dcid, scid, token, payload)

	h, err := Parse(pkt, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if h.Form != FormLong || h.Type != LongInitial {
		t.Fatalf("form/type = %v/%v", h.Form, h.Type)
	}
	if h.Version != Version1 {
		t.Fatalf("version = %x", h.Version)
	}
	if !bytes.Equal(h.DCID, dcid) {
		t.Fatalf("dcid = %x", h.DCID)
	}
	if !bytes.Equal(h.SCID, scid) {
		t.Fatalf("scid = %x", h.SCID)
	}
	if !bytes.Equal(h.Token, token) {
		t.Fatalf("token = %q", h.Token)
	}
	if h.Length != uint64(len(payload)) {
		t.Fatalf("length = %d", h.Length)
	}
	if h.PayloadOffset+int(h.Length) != len(pkt) {
		t.Fatalf("payload offset inconsistent: off=%d length=%d total=%d",
			h.PayloadOffset, h.Length, len(pkt))
	}
}

func TestParseShortHeader(t *testing.T) {
	dcid := []byte{0xde, 0xad, 0xbe, 0xef, 0xca, 0xfe, 0xba, 0xbe}
	// First byte: short form (0x00), fixed bit (0x40), arbitrary protected bits.
	pkt := append([]byte{0x40}, dcid...)
	pkt = append(pkt, 0x01, 0x02, 0x03, 0x04) // fake packet number + payload

	h, err := Parse(pkt, len(dcid))
	if err != nil {
		t.Fatalf("Parse short: %v", err)
	}
	if h.Form != FormShort {
		t.Fatalf("form = %v", h.Form)
	}
	if !bytes.Equal(h.DCID, dcid) {
		t.Fatalf("dcid mismatch")
	}
	if h.PayloadOffset != 1+len(dcid) {
		t.Fatalf("payload offset = %d", h.PayloadOffset)
	}
}

func TestParseShortFixedBitRejected(t *testing.T) {
	pkt := append([]byte{0x00}, bytes.Repeat([]byte{1}, 8)...)
	_, err := Parse(pkt, 8)
	if !errors.Is(err, ErrFixedBit) {
		t.Fatalf("err = %v, want ErrFixedBit", err)
	}
}

func TestParseConnIDTooLong(t *testing.T) {
	// Long header with DCID length 21.
	pkt := []byte{0xc0, 0, 0, 0, 1, 21}
	pkt = append(pkt, bytes.Repeat([]byte{0xff}, 21)...)
	_, err := Parse(pkt, 0)
	if !errors.Is(err, ErrConnIDTooLong) {
		t.Fatalf("err = %v, want ErrConnIDTooLong", err)
	}
}

func TestParseTruncated(t *testing.T) {
	_, err := Parse(nil, 0)
	if !errors.Is(err, ErrShort) {
		t.Fatalf("err = %v, want ErrShort", err)
	}
	// Long header missing the SCID length byte.
	_, err = Parse([]byte{0xc0, 0, 0, 0, 1, 4, 1, 2, 3, 4}, 0)
	if !errors.Is(err, ErrShort) {
		t.Fatalf("short SCID err = %v, want ErrShort", err)
	}
}

func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 63, 64, 16383, 16384, 1<<30 - 1, 1 << 30, 1<<62 - 1}
	for _, v := range cases {
		b := AppendVarint(nil, v)
		got, n, ok := ReadVarint(b)
		if !ok || n != len(b) || got != v {
			t.Fatalf("varint %d round-trip: got=%d n=%d len=%d ok=%v", v, got, n, len(b), ok)
		}
	}
}

func TestVersionNegotiationBuild(t *testing.T) {
	clientDCID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	clientSCID := []byte{9, 10}
	pkt := BuildVersionNegotiation(nil, clientDCID, clientSCID, []uint32{Version1, 0x0a0a0a0a})

	h, err := Parse(pkt, 0)
	if !errors.Is(err, ErrVersionNeg) {
		t.Fatalf("Parse VN err = %v, want ErrVersionNeg", err)
	}
	if h.Version != 0 {
		t.Fatalf("VN version = %x", h.Version)
	}
	// IDs must be swapped relative to the client packet.
	if !bytes.Equal(h.DCID, clientSCID) {
		t.Fatalf("VN DCID = %x, want clientSCID %x", h.DCID, clientSCID)
	}
	if !bytes.Equal(h.SCID, clientDCID) {
		t.Fatalf("VN SCID = %x, want clientDCID %x", h.SCID, clientDCID)
	}
	// Versions list should contain Version1.
	tail := pkt[h.PayloadOffset:]
	if len(tail)%4 != 0 || len(tail) < 4 {
		t.Fatalf("VN tail length = %d", len(tail))
	}
	found := false
	for i := 0; i+4 <= len(tail); i += 4 {
		if binary.BigEndian.Uint32(tail[i:i+4]) == Version1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Version1 not in VN supported list")
	}
}
