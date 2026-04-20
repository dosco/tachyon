package quic

import (
	"context"
	stdtls "crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"tachyon/quic/crypto"
	"tachyon/quic/frame"
	"tachyon/quic/packet"
)

// testClient is the minimum QUIC client needed to drive a handshake
// against the server Endpoint and exchange 1-RTT STREAM frames. Not a
// general-purpose client — no loss recovery, no flow control, no
// migration, no packet-number gaps.
type testClient struct {
	sock *net.UDPConn
	peer *net.UDPAddr

	tls *stdtls.QUICConn

	initialSend, initialRecv *crypto.PacketProtector
	handshakeSend, handshakeRecv *crypto.PacketProtector
	appSend, appRecv *crypto.PacketProtector

	dcid, scid []byte

	pnInitialOut, pnHandshakeOut, pnAppOut uint64

	// Largest seen per space (needed for pn decode on incoming packets).
	pnInitialInLargest, pnHandshakeInLargest, pnAppInLargest uint64
	seenInitial, seenHandshake, seenApp                      bool

	cryptoOffInitial, cryptoOffHandshake uint64
	pendingInitial, pendingHandshake     []byte

	// Stream receive buffer, keyed by stream id.
	streams map[uint64][]byte
	streamFin map[uint64]bool

	handshakeDone bool
}

func newTestClient(t *testing.T, srvAddr net.Addr) *testClient {
	t.Helper()
	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("client ListenUDP: %v", err)
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
	qc.SetTransportParameters(TestClientTransportParams(scid))

	return &testClient{
		sock: sock, peer: peer,
		tls:         qc,
		initialSend: iSend, initialRecv: iRecv,
		dcid: dcid, scid: scid,
		streams:   make(map[uint64][]byte),
		streamFin: make(map[uint64]bool),
	}
}

func (c *testClient) start(ctx context.Context) error {
	if err := c.tls.Start(ctx); err != nil {
		return err
	}
	c.drainTLS()
	return nil
}

func (c *testClient) drainTLS() {
	for {
		ev := c.tls.NextEvent()
		if ev.Kind == stdtls.QUICNoEvent {
			return
		}
		switch ev.Kind {
		case stdtls.QUICWriteData:
			switch ev.Level {
			case stdtls.QUICEncryptionLevelInitial:
				c.pendingInitial = append(c.pendingInitial, ev.Data...)
			case stdtls.QUICEncryptionLevelHandshake:
				c.pendingHandshake = append(c.pendingHandshake, ev.Data...)
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

func (c *testClient) flush(t *testing.T) {
	t.Helper()
	// Initial.
	if len(c.pendingInitial) > 0 {
		payload := frame.AppendCrypto(nil, frame.Crypto{Offset: c.cryptoOffInitial, Data: c.pendingInitial})
		c.cryptoOffInitial += uint64(len(c.pendingInitial))
		c.pendingInitial = nil
		// Pad Initials to 1200 bytes (client requirement).
		padTarget := 1200 - 80
		if len(payload) < padTarget {
			payload = append(payload, make([]byte, padTarget-len(payload))...)
		}
		raw, err := packet.SealInitial(nil, c.initialSend, packet.InitialPacket{
			Version: packet.Version1, DCID: c.dcid, SCID: c.scid,
			PacketNumber: c.pnInitialOut, PacketNumLen: 4, Payload: payload,
		})
		if err != nil {
			t.Fatalf("client SealInitial: %v", err)
		}
		c.pnInitialOut++
		for len(raw) < 1200 {
			raw = append(raw, 0)
		}
		if _, err := c.sock.WriteToUDP(raw, c.peer); err != nil {
			t.Fatalf("client write initial: %v", err)
		}
	}
	// Handshake.
	if len(c.pendingHandshake) > 0 && c.handshakeSend != nil {
		payload := frame.AppendCrypto(nil, frame.Crypto{Offset: c.cryptoOffHandshake, Data: c.pendingHandshake})
		c.cryptoOffHandshake += uint64(len(c.pendingHandshake))
		c.pendingHandshake = nil
		if len(payload) < 4 {
			payload = frame.AppendPadding(payload, 4-len(payload))
		}
		raw, err := sealHandshake(c.handshakeSend, handshakePacket{
			Version: packet.Version1, DCID: c.dcid, SCID: c.scid,
			PacketNumber: c.pnHandshakeOut, PacketNumLen: 4, Payload: payload,
		})
		if err != nil {
			t.Fatalf("client sealHandshake: %v", err)
		}
		c.pnHandshakeOut++
		if _, err := c.sock.WriteToUDP(raw, c.peer); err != nil {
			t.Fatalf("client write handshake: %v", err)
		}
	}
}

// sendStream packages a STREAM frame into a 1-RTT packet.
func (c *testClient) sendStream(t *testing.T, sid uint64, data []byte, fin bool) {
	t.Helper()
	if c.appSend == nil {
		t.Fatalf("no app send keys")
	}
	payload := frame.AppendStream(nil, frame.Stream{StreamID: sid, Offset: 0, Data: data, Fin: fin})
	if len(payload) < 4 {
		payload = frame.AppendPadding(payload, 4-len(payload))
	}
	raw, err := sealShort(c.appSend, c.dcid, c.pnAppOut, 4, payload)
	if err != nil {
		t.Fatalf("sealShort: %v", err)
	}
	c.pnAppOut++
	if _, err := c.sock.WriteToUDP(raw, c.peer); err != nil {
		t.Fatalf("client write app: %v", err)
	}
}

func (c *testClient) readOne(t *testing.T, timeout time.Duration) error {
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
				return errors.New("truncated coalesced packet")
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
					return errors.New("no handshake keys")
				}
				plain, pn, err := c.openLong(dg[:end], h, c.handshakeRecv, c.largest(1))
				if err != nil {
					return err
				}
				c.note(1, pn)
				c.deliverCrypto(plain, stdtls.QUICEncryptionLevelHandshake)
			}
			dg = dg[end:]
		} else {
			// 1-RTT short header. Consumes the rest of the datagram.
			if c.appRecv == nil {
				return errors.New("no app keys")
			}
			plain, pn, err := c.openShort(dg)
			if err != nil {
				return err
			}
			c.note(2, pn)
			if err := frame.Parse(plain, frame.Visitor{
				OnPadding:       func() {},
				OnPing:          func() {},
				OnAck:           func(frame.Ack) error { return nil },
				OnHandshakeDone: func() error { c.handshakeDone = true; return nil },
				OnStream: func(s frame.Stream) error {
					c.streams[s.StreamID] = append(c.streams[s.StreamID], s.Data...)
					if s.Fin {
						c.streamFin[s.StreamID] = true
					}
					return nil
				},
				OnCrypto:          func(frame.Crypto) error { return nil },
				OnConnectionClose: func(frame.ConnectionClose) error { return nil },
			}); err != nil {
				return err
			}
			dg = nil
		}
	}
	c.drainTLS()
	return nil
}

func (c *testClient) deliverCrypto(payload []byte, level stdtls.QUICEncryptionLevel) {
	var data []byte
	_ = frame.Parse(payload, frame.Visitor{
		OnCrypto: func(cr frame.Crypto) error {
			data = append(data, cr.Data...)
			return nil
		},
		OnAck:     func(frame.Ack) error { return nil },
		OnPadding: func() {},
		OnPing:    func() {},
	})
	if len(data) > 0 {
		_ = c.tls.HandleData(level, data)
	}
}

func (c *testClient) openLong(buf []byte, h packet.Header, p *crypto.PacketProtector, expected uint64) ([]byte, uint64, error) {
	pnOffset := h.PayloadOffset
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return nil, 0, packet.ErrShort
	}
	mask, err := p.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return nil, 0, err
	}
	pkt := append([]byte(nil), buf...)
	pkt[0] ^= mask[0] & 0x0f
	pnLen := int(pkt[0]&0x03) + 1
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	truncated := uint64(0)
	for i := 0; i < pnLen; i++ {
		truncated = (truncated << 8) | uint64(pkt[pnOffset+i])
	}
	pn := decodePNCompat(expected, truncated, pnLen)
	aadEnd := pnOffset + pnLen
	ctLen := int(h.Length) - pnLen
	plain, err := p.Open(nil, pkt[:aadEnd], pkt[aadEnd:aadEnd+ctLen], pn)
	return plain, pn, err
}

func (c *testClient) openShort(buf []byte) ([]byte, uint64, error) {
	// DCID length: client's chosen DCID, which equals our scid (4 bytes).
	dcidLen := len(c.scid)
	pnOffset := 1 + dcidLen
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return nil, 0, packet.ErrShort
	}
	mask, err := c.appRecv.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return nil, 0, err
	}
	pkt := append([]byte(nil), buf...)
	pkt[0] ^= mask[0] & 0x1f
	pnLen := int(pkt[0]&0x03) + 1
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	truncated := uint64(0)
	for i := 0; i < pnLen; i++ {
		truncated = (truncated << 8) | uint64(pkt[pnOffset+i])
	}
	pn := decodePNCompat(c.largest(2), truncated, pnLen)
	aadEnd := pnOffset + pnLen
	plain, err := c.appRecv.Open(nil, pkt[:aadEnd], pkt[aadEnd:], pn)
	return plain, pn, err
}

func (c *testClient) largest(space int) uint64 {
	switch space {
	case 0:
		if c.seenInitial {
			return c.pnInitialInLargest
		}
	case 1:
		if c.seenHandshake {
			return c.pnHandshakeInLargest
		}
	case 2:
		if c.seenApp {
			return c.pnAppInLargest
		}
	}
	return 0
}

func (c *testClient) note(space int, pn uint64) {
	switch space {
	case 0:
		if !c.seenInitial || pn > c.pnInitialInLargest {
			c.pnInitialInLargest = pn
		}
		c.seenInitial = true
	case 1:
		if !c.seenHandshake || pn > c.pnHandshakeInLargest {
			c.pnHandshakeInLargest = pn
		}
		c.seenHandshake = true
	case 2:
		if !c.seenApp || pn > c.pnAppInLargest {
			c.pnAppInLargest = pn
		}
		c.seenApp = true
	}
}

// TestStreamEcho runs a full handshake against the server Endpoint
// then opens a bidi stream, sends bytes, and verifies the server's
// accept loop receives them. Server-side echo is handled by the test's
// accept goroutine which writes back what it read.
func TestStreamEcho(t *testing.T) {
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

	// Accept loop: for each new stream, read all bytes and echo them back.
	go func() {
		// Poll the endpoint's stream-accept channel across all connections.
		// Phase 3 scope: endpoint only exposes one active connection at a
		// time via its internal map; we fish it out with a short poll.
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			var cs *connState
			ep.mu.RLock()
			for _, c := range ep.conns {
				cs = c
				break
			}
			ep.mu.RUnlock()
			if cs == nil || !cs.handshakeComplete {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			s, err := cs.AcceptStream(ctx)
			if err != nil {
				return
			}
			go func(s *Stream) {
				buf := make([]byte, 1024)
				for {
					n, fin := s.Read(buf)
					if n > 0 {
						_, _ = s.Write(buf[:n])
					}
					if fin {
						_ = s.CloseWrite()
						_ = cs.Flush()
						return
					}
					if n == 0 {
						<-s.RecvSignal()
					}
				}
			}(s)
		}
	}()

	cli := newTestClient(t, pc.LocalAddr())
	defer cli.sock.Close()

	if err := cli.start(ctx); err != nil {
		t.Fatalf("client start: %v", err)
	}
	cli.flush(t)

	deadline := time.Now().Add(5 * time.Second)
	for !cli.handshakeDone && time.Now().Before(deadline) {
		if err := cli.readOne(t, 500*time.Millisecond); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Nudge the server with an empty 1-RTT packet once keys
				// exist so it can drain HANDSHAKE_DONE.
				if cli.appSend != nil {
					cli.sendStream(t, 0, nil, false)
				}
				continue
			}
			t.Fatalf("readOne: %v", err)
		}
		cli.flush(t)
	}
	if !cli.handshakeDone {
		t.Fatalf("handshake not done")
	}

	// Open client-initiated bidi stream 0 and write a request.
	req := []byte("hello tachyon quic stream")
	cli.sendStream(t, 0, req, true)

	// Expect an echo back.
	echoDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(echoDeadline) {
		if cli.streamFin[0] && string(cli.streams[0]) == string(req) {
			break
		}
		if err := cli.readOne(t, 500*time.Millisecond); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			t.Fatalf("readOne: %v", err)
		}
	}
	if got := string(cli.streams[0]); got != string(req) {
		t.Fatalf("echo mismatch: got %q want %q", got, string(req))
	}
	if !cli.streamFin[0] {
		t.Fatalf("echo never FIN'd")
	}
}
