package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// TestDriverHandshake drives a full TLS 1.3 handshake between two
// Conns back-to-back, with each side's emitted CRYPTO bytes handed to
// the other. No QUIC packet machinery involved — this test just
// confirms the event drain wraps the stdlib QUICConn correctly.
func TestDriverHandshake(t *testing.T) {
	serverTLS := selfSignedConfig(t, "h3")
	clientTLS := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"},
		MinVersion:         tls.VersionTLS13,
	}

	server := NewServer(serverTLS)
	server.SetTransportParameters([]byte{0x00}) // dummy empty-ish params

	cCfg := &tls.QUICConfig{TLSConfig: clientTLS}
	clientRaw := tls.QUICClient(cCfg)
	clientRaw.SetTransportParameters([]byte{0x00})
	client := &Conn{q: clientRaw, cfg: cCfg}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Start(ctx); err != nil {
		t.Fatalf("server.Start: %v", err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatalf("client.Start: %v", err)
	}

	// Pump events back and forth until both sides report HandshakeDone.
	serverDone := false
	clientDone := false
	for i := 0; i < 16 && !(serverDone && clientDone); i++ {
		for _, ev := range client.Events() {
			if ev.Kind == EventHandshakeComplete {
				clientDone = true
			}
			if ev.Kind == EventWriteData {
				if err := server.HandleCrypto(ev.Level, ev.Data); err != nil {
					t.Fatalf("server handle: %v", err)
				}
			}
		}
		for _, ev := range server.Events() {
			if ev.Kind == EventHandshakeComplete {
				serverDone = true
			}
			if ev.Kind == EventWriteData {
				if err := client.HandleCrypto(ev.Level, ev.Data); err != nil {
					t.Fatalf("client handle: %v", err)
				}
			}
		}
	}
	if !(serverDone && clientDone) {
		t.Fatalf("handshake did not complete: serverDone=%v clientDone=%v", serverDone, clientDone)
	}

	state := server.ConnectionState()
	if state.NegotiatedProtocol != "h3" {
		t.Fatalf("alpn = %q, want h3", state.NegotiatedProtocol)
	}

	// 1-RTT resumption: server must be able to issue a NewSessionTicket
	// after handshake completion; the resulting WriteData must land at
	// Application encryption level so it goes out in a 1-RTT packet.
	if err := server.SendSessionTicket(); err != nil {
		t.Fatalf("SendSessionTicket: %v", err)
	}
	sawAppTicket := false
	for _, ev := range server.Events() {
		if ev.Kind == EventWriteData && ev.Level == LevelApplication {
			sawAppTicket = true
		}
	}
	if !sawAppTicket {
		t.Fatalf("no NewSessionTicket WriteData emitted at LevelApplication")
	}
}

func selfSignedConfig(t *testing.T, alpn ...string) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "quic-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}},
		NextProtos: alpn,
		MinVersion: tls.VersionTLS13,
	}
}
