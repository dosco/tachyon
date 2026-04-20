package frame

import (
	"bytes"
	"testing"
)

func TestCryptoRoundTrip(t *testing.T) {
	in := Crypto{Offset: 0, Data: []byte("fake-clienthello-bytes")}
	buf := AppendCrypto(nil, in)

	var got Crypto
	err := Parse(buf, Visitor{
		OnCrypto: func(c Crypto) error {
			got = c
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Offset != in.Offset || !bytes.Equal(got.Data, in.Data) {
		t.Fatalf("mismatch: got %+v want %+v", got, in)
	}
}

func TestAckSingleRange(t *testing.T) {
	buf := AppendAck(nil, 5, 2, 0)
	var got Ack
	err := Parse(buf, Visitor{OnAck: func(a Ack) error { got = a; return nil }})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.LargestAcked != 5 || len(got.AckRanges) != 1 ||
		got.AckRanges[0].Smallest != 2 || got.AckRanges[0].Largest != 5 {
		t.Fatalf("ack = %+v", got)
	}
}

func TestConnectionCloseProtocol(t *testing.T) {
	buf := AppendConnectionClose(nil, 0x0a, 0, "oops")
	var got ConnectionClose
	err := Parse(buf, Visitor{OnConnectionClose: func(c ConnectionClose) error { got = c; return nil }})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.IsApplication || got.ErrorCode != 0x0a || string(got.Reason) != "oops" {
		t.Fatalf("cc = %+v", got)
	}
}

func TestPaddingPingCoexist(t *testing.T) {
	buf := []byte{0x00, 0x00, 0x01, 0x00}
	pads := 0
	pings := 0
	err := Parse(buf, Visitor{
		OnPadding: func() { pads++ },
		OnPing:    func() { pings++ },
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if pads != 3 || pings != 1 {
		t.Fatalf("pads=%d pings=%d", pads, pings)
	}
}

func TestParseUnknownType(t *testing.T) {
	// 0x30 is unassigned.
	buf := []byte{0x30, 0x00}
	err := Parse(buf, Visitor{})
	if err == nil {
		t.Fatal("Parse unknown type: expected error")
	}
}

func TestParseTruncated(t *testing.T) {
	// CRYPTO with declared length beyond buffer.
	buf := []byte{0x06, 0x00, 0x10}
	err := Parse(buf, Visitor{})
	if err == nil {
		t.Fatal("Parse truncated: expected error")
	}
}
