package crypto

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// RFC 9001 Appendix A: Sample Packet Protection
//
// These vectors are the canonical cross-check for any QUIC Initial
// implementation. All constants are copy-pasted from the RFC. If any
// of these tests fail, the derivation is wrong at the cryptographic
// level and no amount of higher-level testing will rescue it.
//
// client_dst_connection_id = 0x8394c8f03e515708

var rfcClientDCID = decodeHex("8394c8f03e515708")

func TestRFC9001_InitialSecret(t *testing.T) {
	// A.1 initial_secret
	want := decodeHex(
		"7db5df06e7a69e432496adedb0085192" +
			"3595221596ae2ae9fb8115c1e9ed0a44",
	)
	got := InitialSecret(rfcClientDCID)
	if !bytes.Equal(got, want) {
		t.Fatalf("initial_secret:\n got  %x\n want %x", got, want)
	}
}

func TestRFC9001_ClientInitialKeys(t *testing.T) {
	// A.1 client initial
	wantSecret := decodeHex("c00cf151ca5be075ed0ebfb5c80323c4" +
		"2d6b7db67881289af4008f1f6c357aea")
	wantKey := decodeHex("1f369613dd76d5467730efcbe3b1a22d")
	wantIV := decodeHex("fa044b2f42a3fd3b46fb255c")
	wantHP := decodeHex("9f50449e04a0e810283a1e9933adedd2")

	client, _, err := DeriveInitialSecrets(rfcClientDCID)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	check(t, "client secret", client.Secret, wantSecret)
	check(t, "client key", client.Key, wantKey)
	check(t, "client iv", client.IV, wantIV)
	check(t, "client hp", client.HP, wantHP)
}

func TestRFC9001_ServerInitialKeys(t *testing.T) {
	// A.1 server initial
	wantSecret := decodeHex("3c199828fd139efd216c155ad844cc81" +
		"fb82fa8d7446fa7d78be803acdda951b")
	wantKey := decodeHex("cf3a5331653c364c88f0f379b6067e37")
	wantIV := decodeHex("0ac1493ca1905853b0bba03e")
	wantHP := decodeHex("c206b8d9b9f0f37644430b490eeaa314")

	_, server, err := DeriveInitialSecrets(rfcClientDCID)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	check(t, "server secret", server.Secret, wantSecret)
	check(t, "server key", server.Key, wantKey)
	check(t, "server iv", server.IV, wantIV)
	check(t, "server hp", server.HP, wantHP)
}

// TestRFC9001_HPMask asserts the client Initial's header-protection
// sample -> mask derivation from Appendix A.2.
//
// Sample is the 16 bytes starting 4 bytes past the start of the
// (protected) packet number in the RFC's example client Initial.
// Expected mask from §A.2:
//   mask = AES-ECB_key=hp(sample)[..5] = 437b9aec36
func TestRFC9001_ClientHPMask(t *testing.T) {
	sample := decodeHex("d1b1c98dd7689fb8ec11d242b123dc9b")
	wantMask := decodeHex("437b9aec36")

	client, _, err := DeriveInitialSecrets(rfcClientDCID)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	pp, err := NewAESGCMProtector(client)
	if err != nil {
		t.Fatalf("NewAESGCMProtector: %v", err)
	}
	got, err := pp.HeaderProtectionMask(sample)
	if err != nil {
		t.Fatalf("HeaderProtectionMask: %v", err)
	}
	if !bytes.Equal(got, wantMask) {
		t.Fatalf("hp mask:\n got  %x\n want %x", got, wantMask)
	}
}

// TestRFC9001_SealOpenRoundtrip does a self-consistency check: seal a
// packet with the server secrets, then open it with the matching
// PacketProtector. Does not assert against the RFC's sample ciphertext
// (which depends on many protected fields we haven't built yet), but
// exercises the nonce derivation and AEAD wiring.
func TestRFC9001_SealOpenRoundtrip(t *testing.T) {
	_, server, err := DeriveInitialSecrets(rfcClientDCID)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	pp, err := NewAESGCMProtector(server)
	if err != nil {
		t.Fatalf("NewAESGCMProtector: %v", err)
	}
	header := []byte{0xc0, 0, 0, 0, 1, 0, 0, 0x40, 0x30}
	payload := []byte("CRYPTO frame bytes go here")
	sealed := pp.Seal(nil, header, payload, 0)
	opened, err := pp.Open(nil, header, sealed, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(opened, payload) {
		t.Fatalf("roundtrip payload mismatch")
	}
	if pp.Overhead() != 16 {
		t.Fatalf("overhead = %d, want 16", pp.Overhead())
	}
	// Corrupt a header byte — open must fail (AEAD authenticates header
	// as associated data).
	badHeader := append([]byte(nil), header...)
	badHeader[0] ^= 1
	if _, err := pp.Open(nil, badHeader, sealed, 0); err == nil {
		t.Fatal("Open succeeded despite header tamper")
	}
}

func TestNonceDerivation(t *testing.T) {
	// Synthetic check: nonce = IV XOR pn-in-low-8-bytes.
	_, server, err := DeriveInitialSecrets(rfcClientDCID)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	pp, err := NewAESGCMProtector(server)
	if err != nil {
		t.Fatalf("NewAESGCMProtector: %v", err)
	}
	var n [12]byte
	pp.nonce(n[:], 0x0102030405060708)
	want := make([]byte, 12)
	copy(want, server.IV)
	want[4] ^= 0x01
	want[5] ^= 0x02
	want[6] ^= 0x03
	want[7] ^= 0x04
	want[8] ^= 0x05
	want[9] ^= 0x06
	want[10] ^= 0x07
	want[11] ^= 0x08
	if !bytes.Equal(n[:], want) {
		t.Fatalf("nonce derivation:\n got  %x\n want %x", n[:], want)
	}
}

func check(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n got  %x\n want %x", name, got, want)
	}
}

func decodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
