package packet_test

import (
	"bytes"
	"testing"

	"tachyon/quic/crypto"
	"tachyon/quic/frame"
	"tachyon/quic/packet"
)

// TestSealOpenInitialRoundtrip builds a server Initial carrying a CRYPTO
// frame, then opens it with the same keys and checks the frame comes
// back intact. This exercises the full pipeline: varint length encoding,
// AEAD seal, header protection apply, header protection strip, AEAD
// open, packet-number recovery.
func TestSealOpenInitialRoundtrip(t *testing.T) {
	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	scid := []byte{9, 9, 9, 9}
	clientSecrets, serverSecrets, err := crypto.DeriveInitialSecrets(dcid)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	_ = clientSecrets

	sealer, err := crypto.NewAESGCMProtector(serverSecrets)
	if err != nil {
		t.Fatalf("NewAESGCMProtector: %v", err)
	}
	opener, err := crypto.NewAESGCMProtector(serverSecrets)
	if err != nil {
		t.Fatalf("NewAESGCMProtector: %v", err)
	}

	payload := frame.AppendCrypto(nil, frame.Crypto{Offset: 0, Data: []byte("server handshake bytes")})
	// Pad to at least 20 bytes so the sample fits (sampleStart = pnOffset+4,
	// need 16 bytes after — so ciphertext must be at least 20 bytes post-
	// AEAD-tag). AEAD tag is 16 bytes, so plaintext >= 4 bytes suffices
	// for a 1-byte pn. Our CRYPTO payload is well over that already.

	raw, err := packet.SealInitial(nil, sealer, packet.InitialPacket{
		Version:      packet.Version1,
		DCID:         dcid,
		SCID:         scid,
		Token:        nil,
		PacketNumber: 0,
		PacketNumLen: 1,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("SealInitial: %v", err)
	}

	h, opened, pn, err := packet.OpenInitial(raw, opener, 0)
	if err != nil {
		t.Fatalf("OpenInitial: %v", err)
	}
	if pn != 0 {
		t.Fatalf("recovered pn = %d, want 0", pn)
	}
	if h.Type != packet.LongInitial || h.Version != packet.Version1 {
		t.Fatalf("header = %+v", h)
	}
	if !bytes.Equal(h.DCID, dcid) || !bytes.Equal(h.SCID, scid) {
		t.Fatalf("cid mismatch")
	}

	// Parse opened frames and make sure we get the CRYPTO back.
	var got frame.Crypto
	err = frame.Parse(opened, frame.Visitor{
		OnCrypto: func(c frame.Crypto) error { got = c; return nil },
	})
	if err != nil {
		t.Fatalf("frame.Parse: %v", err)
	}
	if got.Offset != 0 || string(got.Data) != "server handshake bytes" {
		t.Fatalf("crypto frame = %+v", got)
	}
}

func TestSealPNLengthTwo(t *testing.T) {
	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	_, serverSecrets, err := crypto.DeriveInitialSecrets(dcid)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	pp, err := crypto.NewAESGCMProtector(serverSecrets)
	if err != nil {
		t.Fatalf("NewAESGCMProtector: %v", err)
	}

	payload := frame.AppendPadding(nil, 32)
	raw, err := packet.SealInitial(nil, pp, packet.InitialPacket{
		Version:      packet.Version1,
		DCID:         dcid,
		SCID:         []byte{1, 2, 3, 4},
		PacketNumber: 0x1234,
		PacketNumLen: 2,
		Payload:      payload,
	})
	if err != nil {
		t.Fatalf("SealInitial: %v", err)
	}
	_, _, pn, err := packet.OpenInitial(raw, pp, 0x1233)
	if err != nil {
		t.Fatalf("OpenInitial: %v", err)
	}
	if pn != 0x1234 {
		t.Fatalf("pn = %x, want 0x1234", pn)
	}
}
