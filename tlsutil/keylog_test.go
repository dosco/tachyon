// Round-trip test: run a real TLS 1.3 handshake through the capture +
// HKDF derivation, and verify the server's derived TX key/IV match the
// keys the client would use for its *RX* direction (i.e. that we
// correctly mirror crypto/tls's key schedule).
//
// We don't need a kernel for this — the point is to prove our NSS
// parsing + HKDF-Expand-Label produce bytes that match the AEAD key
// crypto/tls is using internally. We do that indirectly: derive keys
// from both the server capture and a second capture on the client, and
// assert the server's server_to_client secret == client's server_to_
// client secret (same log, both sides see the same labels).

package tlsutil

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"testing"
)

func TestKeyLogCapture_TLS13RoundTrip(t *testing.T) {
	// Self-signed cert so we don't need a real CA.
	cert, err := generateSelfSigned()
	if err != nil {
		t.Fatalf("self-signed: %v", err)
	}

	serverCap := NewCapture()
	clientCap := NewCapture()

	serverCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		KeyLogWriter: serverCap,
	}
	clientCfg := &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		KeyLogWriter:       clientCap,
	}

	// In-memory pipe — no kernel socket needed.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	var sConn, cConn *tls.Conn
	var sErr, cErr error
	go func() {
		defer wg.Done()
		sConn = tls.Server(c1, serverCfg)
		sErr = sConn.Handshake()
		// Echo one byte so the handshake actually runs to completion
		// on both sides (TLS 1.3 finished is half-duplex).
		if sErr == nil {
			_, _ = sConn.Write([]byte{0x42})
		}
	}()
	go func() {
		defer wg.Done()
		cConn = tls.Client(c2, clientCfg)
		cErr = cConn.Handshake()
		if cErr == nil {
			buf := make([]byte, 1)
			_, _ = io.ReadFull(cConn, buf)
		}
	}()
	wg.Wait()

	if sErr != nil || cErr != nil {
		t.Fatalf("handshake: server=%v client=%v", sErr, cErr)
	}

	// Both sides should have logged both TLS 1.3 application traffic
	// secrets.
	ss, err := serverCap.Secrets()
	if err != nil {
		t.Fatalf("server secrets: %v", err)
	}
	cs, err := clientCap.Secrets()
	if err != nil {
		t.Fatalf("client secrets: %v", err)
	}

	// Both captures observed the same handshake, so the secrets must
	// match byte-for-byte.
	if !bytes.Equal(ss.ClientToServer, cs.ClientToServer) {
		t.Errorf("c2s secret mismatch:\n  server=%s\n  client=%s",
			hex.EncodeToString(ss.ClientToServer),
			hex.EncodeToString(cs.ClientToServer))
	}
	if !bytes.Equal(ss.ServerToClient, cs.ServerToClient) {
		t.Errorf("s2c secret mismatch:\n  server=%s\n  client=%s",
			hex.EncodeToString(ss.ServerToClient),
			hex.EncodeToString(cs.ServerToClient))
	}

	// Derive keys + IVs and sanity-check lengths. The actual bytes are
	// non-deterministic (fresh randomness every run) so we only assert
	// structural correctness here.
	cipher, ok := CipherFromSuite(sConn.ConnectionState().CipherSuite)
	if !ok {
		t.Fatalf("unsupported suite for kTLS: 0x%x", sConn.ConnectionState().CipherSuite)
	}
	key := HKDFExpandLabel(cipher.HashFor(), ss.ServerToClient, "key", nil, cipher.KeyLen())
	iv := HKDFExpandLabel(cipher.HashFor(), ss.ServerToClient, "iv", nil, 12)
	if len(key) != cipher.KeyLen() {
		t.Errorf("tx key len: got %d want %d", len(key), cipher.KeyLen())
	}
	if len(iv) != 12 {
		t.Errorf("tx iv len: got %d want 12", len(iv))
	}
	// Non-trivial sanity: derived bytes shouldn't be the secret itself.
	if bytes.Equal(key, ss.ServerToClient[:cipher.KeyLen()]) {
		t.Error("derived key equals first K bytes of secret — HKDF isn't running")
	}
}

func TestHKDFExpandLabel_Vector(t *testing.T) {
	// RFC 8448 §3 (TLS 1.3 key schedule test vectors). We verify one
	// derivation from the published vector to catch label/format bugs.
	//
	//   client_application_traffic_secret_0 =
	//     9e 40 64 6c e7 9a 7f 9d c0 5a f8 88 9b ce 65 52
	//     87 5a fa 0b 06 df 00 87 f7 92 eb b7 c1 75 04 a5
	//
	// Expected key (AES-128-GCM):
	//     17 42 2d da 59 6e d5 d9 ac d8 90 e3 c6 3f 50 51
	// Expected IV:
	//     5b 78 92 3d ee 08 57 90 33 e5 23 d9
	secret, _ := hex.DecodeString(
		"9e40646ce79a7f9dc05af8889bce6552875afa0b06df0087f792ebb7c17504a5")
	wantKey, _ := hex.DecodeString("17422dda596ed5d9acd890e3c63f5051")
	wantIV, _ := hex.DecodeString("5b78923dee08579033e523d9")

	key := HKDFExpandLabel(HashSHA256, secret, "key", nil, 16)
	iv := HKDFExpandLabel(HashSHA256, secret, "iv", nil, 12)

	if !bytes.Equal(key, wantKey) {
		t.Errorf("key mismatch:\n  got  %s\n  want %s",
			hex.EncodeToString(key), hex.EncodeToString(wantKey))
	}
	if !bytes.Equal(iv, wantIV) {
		t.Errorf("iv mismatch:\n  got  %s\n  want %s",
			hex.EncodeToString(iv), hex.EncodeToString(wantIV))
	}
}
