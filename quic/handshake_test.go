package quic

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	stdtls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"tachyon/quic/crypto"
	"tachyon/quic/frame"
	"tachyon/quic/packet"
	qtls "tachyon/quic/tls"
)

// TestHandshakeFirstFlight sends a real client Initial (ClientHello) to
// the server-side Endpoint, reads whatever datagrams come back, and
// confirms the first Initial carries a ServerHello. This is the Phase 2
// wire-level exit criterion: the endpoint can be addressed with a
// real QUIC Initial and produces a well-formed, correctly protected
// reply.
func TestHandshakeFirstFlight(t *testing.T) {
	tlsCfg := selfSignedConfigServer(t)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server ListenPacket: %v", err)
	}
	ep := NewEndpoint(pc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ep.SetTLSConfig(tlsCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ctx) }()

	// Client side.
	cli, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client ListenPacket: %v", err)
	}
	defer cli.Close()

	clientDCID := []byte{0x83, 0x94, 0xc8, 0xf0, 0x3e, 0x51, 0x57, 0x08}
	clientSCID := []byte{0xc1, 0xc2, 0xc3, 0xc4}
	clientSecrets, _, err := crypto.DeriveInitialSecrets(clientDCID)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	// Same keys in both directions: Initial protection uses the sending
	// party's secrets. Client sends with clientSecrets; server's
	// initialRecv matches clientSecrets.
	clientSeal, err := crypto.NewAESGCMProtector(clientSecrets)
	if err != nil {
		t.Fatalf("client sealer: %v", err)
	}

	// Drive a TLS client just far enough to emit the ClientHello.
	clientTLSCfg := &stdtls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"},
		ServerName:         "localhost",
		MinVersion:         stdtls.VersionTLS13,
	}
	clientRaw := stdtls.QUICClient(&stdtls.QUICConfig{TLSConfig: clientTLSCfg})
	clientRaw.SetTransportParameters([]byte{0x01, 0x00}) // throwaway
	if err := clientRaw.Start(ctx); err != nil {
		t.Fatalf("client tls.Start: %v", err)
	}
	var clientHello []byte
	for {
		ev := clientRaw.NextEvent()
		if ev.Kind == stdtls.QUICNoEvent {
			break
		}
		if ev.Kind == stdtls.QUICWriteData && ev.Level == stdtls.QUICEncryptionLevelInitial {
			clientHello = append(clientHello, ev.Data...)
		}
	}
	if len(clientHello) == 0 {
		t.Fatalf("no ClientHello emitted")
	}

	payload := frame.AppendCrypto(nil, frame.Crypto{Offset: 0, Data: clientHello})
	// RFC 9000 §14.1: UDP payload carrying an Initial from a client must
	// be at least 1200 bytes.
	raw, err := packet.SealInitial(nil, clientSeal, packet.InitialPacket{
		Version:      packet.Version1,
		DCID:         clientDCID,
		SCID:         clientSCID,
		PacketNumber: 0,
		PacketNumLen: 4,
		Payload:      padTo(payload, 1200-64),
	})
	if err != nil {
		t.Fatalf("client SealInitial: %v", err)
	}
	for len(raw) < 1200 {
		raw = append(raw, 0)
	}

	if _, err := cli.WriteTo(raw, pc.LocalAddr()); err != nil {
		t.Fatalf("client WriteTo: %v", err)
	}

	// Expect at least one reply datagram. The server sends Initial
	// (ACK + ServerHello) and, assuming handshake keys are installed,
	// Handshake packets too — we only inspect the first Initial here.
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	foundServerHello := false
	_, serverSecrets, _ := crypto.DeriveInitialSecrets(clientDCID)
	serverOpen, err := crypto.NewAESGCMProtector(serverSecrets)
	if err != nil {
		t.Fatalf("server opener: %v", err)
	}
	for attempts := 0; attempts < 4 && !foundServerHello; attempts++ {
		n, _, err := cli.ReadFrom(buf)
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		// Walk coalesced long packets within the datagram.
		dg := buf[:n]
		for len(dg) > 0 {
			if !packet.IsLongHeader(dg[0]) {
				break
			}
			h, err := packet.Parse(dg, 0)
			if err != nil {
				break
			}
			end := h.PayloadOffset + int(h.Length)
			if end > len(dg) {
				break
			}
			if h.Type == packet.LongInitial {
				_, plain, _, err := packet.OpenInitial(dg[:end], serverOpen, 0)
				if err == nil {
					_ = frame.Parse(plain, frame.Visitor{
						OnCrypto: func(c frame.Crypto) error {
							// First byte of a ServerHello TLS record is
							// handshake type 0x02.
							if len(c.Data) > 0 && c.Data[0] == 0x02 {
								foundServerHello = true
							}
							return nil
						},
						OnAck:     func(frame.Ack) error { return nil },
						OnPadding: func() {},
						OnPing:    func() {},
					})
				}
			}
			dg = dg[end:]
		}
	}
	if !foundServerHello {
		t.Fatalf("did not observe ServerHello in server reply")
	}

	cancel()
	<-done
}

func padTo(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(b, make([]byte, n-len(b))...)
}

func selfSignedConfigServer(t *testing.T) *stdtls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return &stdtls.Config{
		Certificates: []stdtls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
		}},
		NextProtos: []string{"h3"},
		MinVersion: stdtls.VersionTLS13,
	}
}

var _ = qtls.LevelInitial
