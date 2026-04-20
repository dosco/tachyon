package http3_test

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

	"tachyon/http3"
	"tachyon/http3/frame"
	"tachyon/http3/qpack"
	"tachyon/quic"
	"tachyon/quic/crypto"
	qframe "tachyon/quic/frame"
	"tachyon/quic/packet"
)

// TestHTTP3HelloWorld wires the full stack: tachyon/quic Endpoint
// terminates TLS, http3.Serve accepts a bidi stream, the handler replies
// with :status 200 and "hello tachyon". A minimal in-test client
// drives the exchange — same scaffolding as quic.TestStreamEcho,
// extended to emit HTTP/3 HEADERS/DATA on the stream.
func TestHTTP3HelloWorld(t *testing.T) {
	tlsCfg := selfSignedConfig(t)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("server ListenPacket: %v", err)
	}
	ep := quic.NewEndpoint(pc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ep.SetTLSConfig(tlsCfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ctx) }()

	// Server-side: accept the connection, run HTTP/3 Serve in the
	// background.
	go func() {
		c, err := ep.AcceptConn(ctx)
		if err != nil {
			return
		}
		_ = http3.ServeConn(ctx, c, func(_ context.Context, req *http3.Request, rw *http3.ResponseWriter) {
			rw.Status = 200
			rw.SetHeader("content-type", "text/plain")
			_, _ = rw.Write([]byte("hello tachyon"))
			_ = req
		})
	}()

	cli := newH3TestClient(t, pc.LocalAddr())
	defer cli.Close()

	cli.completeHandshake(t, ctx)
	cli.sendRequest(t, []qpack.Field{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: "localhost"},
		{Name: ":path", Value: "/"},
	})

	status, body := cli.readResponse(t)
	if status != "200" {
		t.Fatalf(":status = %q, want 200", status)
	}
	if string(body) != "hello tachyon" {
		t.Fatalf("body = %q, want hello tachyon", body)
	}
}

// ---- test-only HTTP/3 client below ----
//
// Keeps the quic/echo_test.go client scaffolding but encodes
// requests/responses through http3/frame and qpack.

type h3client struct {
	sock *net.UDPConn
	peer *net.UDPAddr

	tls *stdtls.QUICConn

	initialSend, initialRecv         *crypto.PacketProtector
	handshakeSend, handshakeRecv     *crypto.PacketProtector
	appSend, appRecv                 *crypto.PacketProtector
	dcid, scid                       []byte
	pnInitialOut, pnHsOut, pnAppOut  uint64
	pnIInLarge, pnHInLarge, pnAInLarge uint64
	seenI, seenH, seenA              bool
	coInit, coHs                     uint64
	pendI, pendH                     []byte

	streams   map[uint64][]byte
	streamFin map[uint64]bool

	handshakeDone bool
}

func newH3TestClient(t *testing.T, srvAddr net.Addr) *h3client {
	t.Helper()
	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	peer := srvAddr.(*net.UDPAddr)
	dcid := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	scid := []byte{0xaa, 0xbb, 0xcc, 0xdd}
	clientSec, serverSec, err := crypto.DeriveInitialSecrets(dcid)
	if err != nil {
		t.Fatalf("DeriveInitialSecrets: %v", err)
	}
	iSend, _ := crypto.NewAESGCMProtector(clientSec)
	iRecv, _ := crypto.NewAESGCMProtector(serverSec)
	qc := stdtls.QUICClient(&stdtls.QUICConfig{TLSConfig: &stdtls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h3"},
		ServerName:         "localhost",
		MinVersion:         stdtls.VersionTLS13,
	}})
	qc.SetTransportParameters(quic.TestClientTransportParams(scid))
	return &h3client{
		sock: sock, peer: peer, tls: qc,
		initialSend: iSend, initialRecv: iRecv,
		dcid: dcid, scid: scid,
		streams:   make(map[uint64][]byte),
		streamFin: make(map[uint64]bool),
	}
}

func (c *h3client) Close() { _ = c.sock.Close() }

func (c *h3client) drainTLS() {
	for {
		ev := c.tls.NextEvent()
		if ev.Kind == stdtls.QUICNoEvent {
			return
		}
		switch ev.Kind {
		case stdtls.QUICWriteData:
			switch ev.Level {
			case stdtls.QUICEncryptionLevelInitial:
				c.pendI = append(c.pendI, ev.Data...)
			case stdtls.QUICEncryptionLevelHandshake:
				c.pendH = append(c.pendH, ev.Data...)
			}
		case stdtls.QUICSetReadSecret:
			p, _ := crypto.NewAESGCMProtector(crypto.SecretsFromTLS(ev.Suite, ev.Data))
			switch ev.Level {
			case stdtls.QUICEncryptionLevelHandshake:
				c.handshakeRecv = p
			case stdtls.QUICEncryptionLevelApplication:
				c.appRecv = p
			}
		case stdtls.QUICSetWriteSecret:
			p, _ := crypto.NewAESGCMProtector(crypto.SecretsFromTLS(ev.Suite, ev.Data))
			switch ev.Level {
			case stdtls.QUICEncryptionLevelHandshake:
				c.handshakeSend = p
			case stdtls.QUICEncryptionLevelApplication:
				c.appSend = p
			}
		case stdtls.QUICHandshakeDone:
			c.handshakeDone = true
		}
	}
}

func (c *h3client) flush(t *testing.T) {
	t.Helper()
	if len(c.pendI) > 0 {
		payload := qframe.AppendCrypto(nil, qframe.Crypto{Offset: c.coInit, Data: c.pendI})
		c.coInit += uint64(len(c.pendI))
		c.pendI = nil
		padTarget := 1200 - 80
		if len(payload) < padTarget {
			payload = append(payload, make([]byte, padTarget-len(payload))...)
		}
		raw, err := packet.SealInitial(nil, c.initialSend, packet.InitialPacket{
			Version: packet.Version1, DCID: c.dcid, SCID: c.scid,
			PacketNumber: c.pnInitialOut, PacketNumLen: 4, Payload: payload,
		})
		if err != nil {
			t.Fatalf("SealInitial: %v", err)
		}
		c.pnInitialOut++
		for len(raw) < 1200 {
			raw = append(raw, 0)
		}
		_, _ = c.sock.WriteToUDP(raw, c.peer)
	}
	if len(c.pendH) > 0 && c.handshakeSend != nil {
		payload := qframe.AppendCrypto(nil, qframe.Crypto{Offset: c.coHs, Data: c.pendH})
		c.coHs += uint64(len(c.pendH))
		c.pendH = nil
		if len(payload) < 4 {
			payload = qframe.AppendPadding(payload, 4-len(payload))
		}
		raw, err := quicSealHandshake(c.handshakeSend, c.dcid, c.scid, c.pnHsOut, 4, payload)
		if err != nil {
			t.Fatalf("handshake seal: %v", err)
		}
		c.pnHsOut++
		_, _ = c.sock.WriteToUDP(raw, c.peer)
	}
}

func (c *h3client) completeHandshake(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := c.tls.Start(ctx); err != nil {
		t.Fatalf("tls.Start: %v", err)
	}
	c.drainTLS()
	c.flush(t)
	deadline := time.Now().Add(5 * time.Second)
	for !c.handshakeDone && time.Now().Before(deadline) {
		if err := c.readOne(t, 500*time.Millisecond); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if c.appSend != nil {
					c.sendRawStream(t, 0, nil, false)
				}
				continue
			}
			t.Fatalf("readOne: %v", err)
		}
		c.flush(t)
	}
	if !c.handshakeDone {
		t.Fatalf("handshake not done")
	}
}

func (c *h3client) sendRequest(t *testing.T, fields []qpack.Field) {
	t.Helper()
	block := qpack.Encode(nil, fields)
	payload := frame.AppendFrame(nil, frame.TypeHeaders, block)
	c.sendRawStream(t, 0, payload, true)
}

func (c *h3client) sendRawStream(t *testing.T, sid uint64, data []byte, fin bool) {
	t.Helper()
	payload := qframe.AppendStream(nil, qframe.Stream{StreamID: sid, Offset: 0, Data: data, Fin: fin})
	if len(payload) < 4 {
		payload = qframe.AppendPadding(payload, 4-len(payload))
	}
	raw, err := quicSealShort(c.appSend, c.dcid, c.pnAppOut, 4, payload)
	if err != nil {
		t.Fatalf("sealShort: %v", err)
	}
	c.pnAppOut++
	_, _ = c.sock.WriteToUDP(raw, c.peer)
}

func (c *h3client) readResponse(t *testing.T) (status string, body []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !c.streamFin[0] {
		if err := c.readOne(t, 500*time.Millisecond); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			t.Fatalf("readOne: %v", err)
		}
	}
	if !c.streamFin[0] {
		t.Fatalf("stream 0 did not FIN")
	}
	raw := c.streams[0]
	for len(raw) > 0 {
		f, n, err := frame.Parse(raw)
		if err != nil {
			t.Fatalf("h3 frame parse: %v", err)
		}
		raw = raw[n:]
		switch f.Type {
		case frame.TypeHeaders:
			fs, err := qpack.Decode(f.Payload)
			if err != nil {
				t.Fatalf("qpack decode: %v", err)
			}
			for _, fd := range fs {
				if fd.Name == ":status" {
					status = fd.Value
				}
			}
		case frame.TypeData:
			body = append(body, f.Payload...)
		}
	}
	return status, body
}

func (c *h3client) readOne(t *testing.T, timeout time.Duration) error {
	t.Helper()
	_ = c.sock.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 4096)
	n, _, err := c.sock.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	dg := buf[:n]
	for len(dg) > 0 {
		if packet.IsLongHeader(dg[0]) {
			h, err := packet.Parse(dg, 0)
			if err != nil {
				return err
			}
			end := h.PayloadOffset + int(h.Length)
			if end > len(dg) {
				return io.ErrShortBuffer
			}
			switch h.Type {
			case packet.LongInitial:
				_, plain, pn, err := packet.OpenInitial(dg[:end], c.initialRecv, c.largest(0))
				if err != nil {
					return err
				}
				c.note(0, pn)
				c.deliverCrypto(plain, stdtls.QUICEncryptionLevelInitial)
			case packet.LongHandshake:
				if c.handshakeRecv == nil {
					return nil
				}
				plain, pn, err := quicOpenLong(dg[:end], h, c.handshakeRecv, c.largest(1))
				if err != nil {
					return err
				}
				c.note(1, pn)
				c.deliverCrypto(plain, stdtls.QUICEncryptionLevelHandshake)
			}
			dg = dg[end:]
		} else {
			if c.appRecv == nil {
				return nil
			}
			plain, pn, err := quicOpenShort(c.appRecv, c.scid, dg, c.largest(2))
			if err != nil {
				return err
			}
			c.note(2, pn)
			if err := qframe.Parse(plain, qframe.Visitor{
				OnPadding:       func() {},
				OnPing:          func() {},
				OnAck:           func(qframe.Ack) error { return nil },
				OnHandshakeDone: func() error { c.handshakeDone = true; return nil },
				OnStream: func(s qframe.Stream) error {
					c.streams[s.StreamID] = append(c.streams[s.StreamID], s.Data...)
					if s.Fin {
						c.streamFin[s.StreamID] = true
					}
					return nil
				},
				OnCrypto:          func(qframe.Crypto) error { return nil },
				OnConnectionClose: func(qframe.ConnectionClose) error { return nil },
			}); err != nil {
				return err
			}
			dg = nil
		}
	}
	c.drainTLS()
	return nil
}

func (c *h3client) deliverCrypto(payload []byte, level stdtls.QUICEncryptionLevel) {
	var data []byte
	_ = qframe.Parse(payload, qframe.Visitor{
		OnCrypto: func(cr qframe.Crypto) error { data = append(data, cr.Data...); return nil },
		OnAck:    func(qframe.Ack) error { return nil },
		OnPadding: func() {},
		OnPing:    func() {},
	})
	if len(data) > 0 {
		_ = c.tls.HandleData(level, data)
	}
}

func (c *h3client) largest(space int) uint64 {
	switch space {
	case 0:
		if c.seenI {
			return c.pnIInLarge
		}
	case 1:
		if c.seenH {
			return c.pnHInLarge
		}
	case 2:
		if c.seenA {
			return c.pnAInLarge
		}
	}
	return 0
}

func (c *h3client) note(space int, pn uint64) {
	switch space {
	case 0:
		if !c.seenI || pn > c.pnIInLarge {
			c.pnIInLarge = pn
		}
		c.seenI = true
	case 1:
		if !c.seenH || pn > c.pnHInLarge {
			c.pnHInLarge = pn
		}
		c.seenH = true
	case 2:
		if !c.seenA || pn > c.pnAInLarge {
			c.pnAInLarge = pn
		}
		c.seenA = true
	}
}

// --- server-side TLS config ---

func selfSignedConfig(t *testing.T) *stdtls.Config {
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

// These helpers are re-exported from quic/ via the init below so we
// don't have to duplicate the wire protection code. They're test-only,
// so we keep them unexported by cross-compiling their bodies inline.

var (
	quicSealHandshake func(p *crypto.PacketProtector, dcid, scid []byte, pn uint64, pnLen int, payload []byte) ([]byte, error)
	quicSealShort     func(p *crypto.PacketProtector, dcid []byte, pn uint64, pnLen int, payload []byte) ([]byte, error)
	quicOpenLong      func(buf []byte, h packet.Header, p *crypto.PacketProtector, expected uint64) ([]byte, uint64, error)
	quicOpenShort     func(p *crypto.PacketProtector, scid, buf []byte, expected uint64) ([]byte, uint64, error)
)

func init() {
	quicSealHandshake = quic.TestSealHandshake
	quicSealShort = quic.TestSealShort
	quicOpenLong = quic.TestOpenLong
	quicOpenShort = quic.TestOpenShort
}
