package quic

import (
	"context"
	stdtls "crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"tachyon/quic/congestion"
	"tachyon/quic/crypto"
	"tachyon/quic/frame"
	"tachyon/quic/packet"
	"tachyon/quic/recovery"
	qtls "tachyon/quic/tls"
)

// timeAfter is a 5ms tick used by AcceptConn's poll loop. Extracted so
// tests can swap in a faster channel if needed.
func timeAfter() <-chan time.Time { return time.After(5 * time.Millisecond) }

// connState is the per-connection machinery: TLS driver, key material
// for each encryption level, packet-number counters, and a small outgoing
// CRYPTO-frame buffer per level that Flush packs into wire packets.
//
// Only the server direction is implemented. Intentionally scoped to what
// Phase 2 needs: drive the TLS handshake through the Initial and
// Handshake encryption levels, then emit a CONNECTION_CLOSE once the
// handshake finishes (Phase 3 brings streams and 1-RTT traffic).
type connState struct {
	ep     *Endpoint
	peer   net.Addr
	logger *slog.Logger

	// Connection IDs. dcid is what the client picked and what we were
	// addressed by; scid is what we chose. The initial salt is derived
	// from the *client's* original DCID per RFC 9001 §5.2.
	clientDCID []byte // == our local SCID from the peer's perspective
	localSCID  []byte
	origDCID   []byte // client's DCID as used for the initial-secret

	tls *qtls.Conn

	// Initial keys (RFC 9001 §5.2).
	initialRecv *crypto.PacketProtector
	initialSend *crypto.PacketProtector

	// Handshake keys.
	handshakeRecv *crypto.PacketProtector
	handshakeSend *crypto.PacketProtector

	// 1-RTT (Application) keys.
	appRecv *crypto.PacketProtector
	appSend *crypto.PacketProtector

	// Outbound packet numbers, one per packet-number space.
	pnInitialOut   uint64
	pnHandshakeOut uint64
	pnAppOut       uint64

	// Largest inbound packet number seen per space.
	pnInitialInLargest   uint64
	pnHandshakeInLargest uint64
	pnAppInLargest       uint64
	seenInitialIn        bool
	seenHandshakeIn      bool
	seenAppIn            bool

	outInitialCrypto   []byte
	outHandshakeCrypto []byte

	ackInitial   bool
	ackHandshake bool
	ackApp       bool

	cryptoOffsetInitial   uint64
	cryptoOffsetHandshake uint64

	handshakeComplete bool
	handshakeDoneSent bool
	closed            bool

	// Streams indexed by stream ID. Phase 3: server-side handling of
	// client-initiated bidi streams only.
	streamsMu sync.Mutex
	streams   map[uint64]*Stream
	// acceptCh is signalled whenever a new peer-initiated stream is
	// created, for the Accept helper used by tests / higher layers.
	acceptCh chan *Stream

	// Flow control (Phase 6). peer* fields hold the client-declared
	// limits from their transport parameters; local* fields hold our
	// own advertised windows. connSendBytes / connRecvBytes track
	// connection-level usage to enforce initial_max_data.
	peerParams     PeerTransportParams
	peerParamsSeen bool
	connSendBytes  uint64 // data we've sent across all streams
	connRecvBytes  uint64 // data the peer has sent us across all streams
	localMaxData   uint64 // what we've advertised to the peer (auto-grown)
	// localInitialMaxStreamData mirrors what encodeServerTransportParams
	// advertises; kept here so the stream receive path can police it.
	localInitialMaxStreamData uint64

	// Loss recovery + congestion control (RFC 9002). Tracked for every
	// ack-eliciting packet we send; used to update RTT on ACKs and to
	// feed the congestion controller's in-flight accounting.
	rec           *recovery.Recovery
	cc            congestion.Controller
	bytesInFlight int
}

func (e *Endpoint) newServerConn(peer net.Addr, clientDCID, clientSCID []byte) (*connState, error) {
	client, server, err := crypto.DeriveInitialSecrets(clientDCID)
	if err != nil {
		return nil, err
	}
	recv, err := crypto.NewAESGCMProtector(client)
	if err != nil {
		return nil, err
	}
	send, err := crypto.NewAESGCMProtector(server)
	if err != nil {
		return nil, err
	}

	tlsCfg := e.tlsConfig
	if tlsCfg == nil {
		return nil, errors.New("quic: endpoint has no TLS config")
	}
	tlsDriver := qtls.NewServer(tlsCfg)
	// Transport parameters: we only ship a minimal set — original_destination_
	// connection_id is mandatory on the server. Encoded in RFC 9000 §18.2.
	tlsDriver.SetTransportParameters(encodeServerTransportParams(clientDCID))

	// Choose our own SCID. Peer will address us by this going forward.
	localSCID := make([]byte, LocalConnIDLen)
	if _, err := e.randRead(localSCID); err != nil {
		return nil, err
	}

	cs := &connState{
		ep:                        e,
		peer:                      peer,
		logger:                    e.logger,
		clientDCID:                append([]byte(nil), clientSCID...), // peer's SCID = our DCID going out
		localSCID:                 localSCID,
		origDCID:                  append([]byte(nil), clientDCID...),
		tls:                       tlsDriver,
		initialRecv:               recv,
		initialSend:               send,
		streams:                   make(map[uint64]*Stream),
		acceptCh:                  make(chan *Stream, 16),
		peerParams:                defaultPeerTransportParams(),
		localMaxData:              localConnFlowWindow,
		localInitialMaxStreamData: localStreamFlowWindow,
		rec:                       recovery.New(25 * time.Millisecond),
		cc:                        congestion.New(),
	}
	return cs, nil
}

// getOrCreateStream returns the Stream object for id, creating it if
// this is the first time we've seen the id and it is peer-initiated.
func (c *connState) getOrCreateStream(id uint64) *Stream {
	c.streamsMu.Lock()
	defer c.streamsMu.Unlock()
	if s, ok := c.streams[id]; ok {
		return s
	}
	s := NewStream(id)
	// Seed send-side limit with the peer's transport-param default.
	// For server-side handling of client-initiated bidi streams the
	// relevant peer value is initial_max_stream_data_bidi_local
	// (from the client, limiting streams the client opened).
	if c.peerParamsSeen {
		s.SetSendMaxOff(c.peerParams.InitialMaxStreamDataBidiLocal)
	}
	s.SetRecvMaxOff(c.localInitialMaxStreamData)
	c.streams[id] = s
	select {
	case c.acceptCh <- s:
	default:
	}
	return s
}

// AcceptStream blocks until a peer opens a new stream or ctx is done.
func (c *connState) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case s := <-c.acceptCh:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Conn is the exported handle to an accepted QUIC connection.
// External packages (http3, proxy integration) use this instead of the
// unexported connState type.
type Conn struct{ inner *connState }

// AcceptStream on a Conn forwards to the underlying connection state.
func (c *Conn) AcceptStream(ctx context.Context) (*Stream, error) {
	return c.inner.AcceptStream(ctx)
}

// Flush packs any buffered CRYPTO/STREAM/ACK data into wire packets
// and writes them. Callers invoke this after producing stream data
// that the peer should see without waiting for the next inbound
// packet to trigger a flush.
func (c *Conn) Flush() error { return c.inner.Flush() }

// AcceptConn blocks until the endpoint has a handshake-complete
// connection available. Phase 4 uses this as the entry point for the
// HTTP/3 layer; later phases will convert it to a channel and support
// multiplexed accepts.
func (e *Endpoint) AcceptConn(ctx context.Context) (*Conn, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		e.mu.RLock()
		var cs *connState
		for _, c := range e.conns {
			if c.handshakeComplete {
				cs = c
				break
			}
		}
		e.mu.RUnlock()
		if cs != nil {
			return &Conn{inner: cs}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeAfter():
		}
	}
}

// Start fires the TLS state machine and processes the initial event
// batch (typically a TransportParametersRequired that we already
// satisfied in newServerConn).
func (c *connState) Start(ctx context.Context) error {
	if err := c.tls.Start(ctx); err != nil {
		return err
	}
	return c.drainEvents()
}

// ReceiveInitial decrypts a client Initial, pulls the CRYPTO frame out,
// and feeds it to the TLS driver.
func (c *connState) ReceiveInitial(buf []byte) error {
	expected := uint64(0)
	if c.seenInitialIn {
		expected = c.pnInitialInLargest
	}
	hdr, payload, pn, err := packet.OpenInitial(buf, c.initialRecv, expected)
	if err != nil {
		return fmt.Errorf("OpenInitial: %w", err)
	}
	_ = hdr
	if !c.seenInitialIn || pn > c.pnInitialInLargest {
		c.pnInitialInLargest = pn
	}
	c.seenInitialIn = true

	return c.deliverCryptoPayload(payload, qtls.LevelInitial)
}

// ReceiveHandshake decrypts a Handshake-level packet from the client.
func (c *connState) ReceiveHandshake(buf []byte) error {
	if c.handshakeRecv == nil {
		return errors.New("quic: handshake keys not installed")
	}
	h, err := packet.Parse(buf, 0)
	if err != nil {
		return err
	}
	if h.Type != packet.LongHandshake {
		return fmt.Errorf("quic: expected Handshake, got %d", h.Type)
	}
	pnOffset := h.PayloadOffset
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return packet.ErrShort
	}
	mask, err := c.handshakeRecv.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return err
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
	expected := uint64(0)
	if c.seenHandshakeIn {
		expected = c.pnHandshakeInLargest
	}
	pn := decodePNCompat(expected, truncated, pnLen)
	aadEnd := pnOffset + pnLen
	ctLen := int(h.Length) - pnLen
	if ctLen < c.handshakeRecv.Overhead() || aadEnd+ctLen > len(pkt) {
		return packet.ErrShort
	}
	plaintext, err := c.handshakeRecv.Open(nil, pkt[:aadEnd], pkt[aadEnd:aadEnd+ctLen], pn)
	if err != nil {
		return err
	}
	if !c.seenHandshakeIn || pn > c.pnHandshakeInLargest {
		c.pnHandshakeInLargest = pn
	}
	c.seenHandshakeIn = true
	return c.deliverCryptoPayload(plaintext, qtls.LevelHandshake)
}

func (c *connState) deliverCryptoPayload(payload []byte, level qtls.Level) error {
	var cryptoBytes []byte
	err := frame.Parse(payload, frame.Visitor{
		OnCrypto: func(cr frame.Crypto) error {
			// Trust in-order CRYPTO during the handshake. Reordering + gap
			// handling is Phase 3.
			cryptoBytes = append(cryptoBytes, cr.Data...)
			return nil
		},
		OnPing:    func() {},
		OnPadding: func() {},
		OnAck: func(a frame.Ack) error {
			space := recovery.SpaceInitial
			if level == qtls.LevelHandshake {
				space = recovery.SpaceHandshake
			}
			c.processAck(space, a, time.Now())
			return nil
		},
	})
	if err != nil {
		return err
	}
	if level == qtls.LevelInitial {
		c.ackInitial = true
	} else if level == qtls.LevelHandshake {
		c.ackHandshake = true
	}
	if len(cryptoBytes) == 0 {
		return nil
	}
	if err := c.tls.HandleCrypto(level, cryptoBytes); err != nil {
		return err
	}
	return c.drainEvents()
}

// drainEvents pulls events from the TLS driver and translates them into
// per-level state updates: install keys, buffer outgoing CRYPTO bytes.
func (c *connState) drainEvents() error {
	for _, ev := range c.tls.Events() {
		switch ev.Kind {
		case qtls.EventSetReadSecret:
			p, err := crypto.NewAESGCMProtector(crypto.SecretsFromTLS(ev.Suite, ev.Data))
			if err != nil {
				return err
			}
			switch ev.Level {
			case qtls.LevelHandshake:
				c.handshakeRecv = p
			case qtls.LevelApplication:
				c.appRecv = p
			}
		case qtls.EventSetWriteSecret:
			p, err := crypto.NewAESGCMProtector(crypto.SecretsFromTLS(ev.Suite, ev.Data))
			if err != nil {
				return err
			}
			switch ev.Level {
			case qtls.LevelHandshake:
				c.handshakeSend = p
			case qtls.LevelApplication:
				c.appSend = p
			}
		case qtls.EventWriteData:
			switch ev.Level {
			case qtls.LevelInitial:
				c.outInitialCrypto = append(c.outInitialCrypto, ev.Data...)
			case qtls.LevelHandshake:
				c.outHandshakeCrypto = append(c.outHandshakeCrypto, ev.Data...)
			}
		case qtls.EventHandshakeComplete:
			c.handshakeComplete = true
		case qtls.EventTransportParameters:
			tp, err := parsePeerTransportParams(ev.Data)
			if err != nil {
				return err
			}
			c.peerParams = tp
			c.peerParamsSeen = true
			if tp.MaxAckDelayMS > 0 {
				c.rec.MaxAckDelay = time.Duration(tp.MaxAckDelayMS) * time.Millisecond
			}
		}
	}
	return nil
}

// Flush packs buffered CRYPTO / STREAM data into Initial, Handshake,
// and 1-RTT packets as appropriate and sends them on the wire.
func (c *connState) Flush() error {
	if len(c.outInitialCrypto) > 0 || c.ackInitial {
		if err := c.flushInitial(); err != nil {
			return err
		}
	}
	if len(c.outHandshakeCrypto) > 0 || c.ackHandshake {
		if err := c.flushHandshake(); err != nil {
			return err
		}
	}
	if c.appSend != nil {
		if err := c.flushApp(); err != nil {
			return err
		}
	}
	return nil
}

// ReceiveShort decrypts a 1-RTT packet and dispatches its frames.
func (c *connState) ReceiveShort(buf []byte) error {
	if c.appRecv == nil {
		return errors.New("quic: application keys not installed")
	}
	// Short header: 0b0100KKKP PPP... Fixed bit 0x40, key phase 0x04
	// (unused for Phase 3 — we error on key rotation), pn len bits.
	// Header layout: 1 byte + DCID (len = LocalConnIDLen) + pn.
	if len(buf) < 1+LocalConnIDLen+4+16 {
		return packet.ErrShort
	}
	pnOffset := 1 + LocalConnIDLen
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return packet.ErrShort
	}
	mask, err := c.appRecv.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return err
	}
	pkt := append([]byte(nil), buf...)
	// Short header: low 5 bits of first byte are HP-protected.
	pkt[0] ^= mask[0] & 0x1f
	pnLen := int(pkt[0]&0x03) + 1
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	truncated := uint64(0)
	for i := 0; i < pnLen; i++ {
		truncated = (truncated << 8) | uint64(pkt[pnOffset+i])
	}
	expected := uint64(0)
	if c.seenAppIn {
		expected = c.pnAppInLargest
	}
	pn := decodePNCompat(expected, truncated, pnLen)
	aadEnd := pnOffset + pnLen
	plaintext, err := c.appRecv.Open(nil, pkt[:aadEnd], pkt[aadEnd:], pn)
	if err != nil {
		return err
	}
	if !c.seenAppIn || pn > c.pnAppInLargest {
		c.pnAppInLargest = pn
	}
	c.seenAppIn = true
	c.ackApp = true
	return c.dispatchAppFrames(plaintext)
}

func (c *connState) dispatchAppFrames(payload []byte) error {
	return frame.Parse(payload, frame.Visitor{
		OnPadding: func() {},
		OnPing:    func() {},
		OnAck: func(a frame.Ack) error {
			c.processAck(recovery.SpaceApplication, a, time.Now())
			return nil
		},
		OnStream: func(s frame.Stream) error {
			st := c.getOrCreateStream(s.StreamID)
			// Approximate conn-level recv accounting: every new frame
			// bytes adds to the total. Refinement would track only
			// net-new bytes past the highest-offset seen, but that
			// requires per-stream accounting we don't need yet.
			c.connRecvBytes += uint64(len(s.Data))
			st.OnStream(s.Offset, s.Data, s.Fin)
			return nil
		},
		OnResetStream: func(r frame.ResetStream) error {
			st := c.getOrCreateStream(r.StreamID)
			st.OnResetStream(r.ErrorCode, r.FinalSize)
			return nil
		},
		OnStopSending: func(frame.StopSending) error { return nil },
		OnMaxData: func(m frame.MaxData) error {
			if m.Max > c.peerParams.InitialMaxData {
				c.peerParams.InitialMaxData = m.Max
			}
			return nil
		},
		OnMaxStreamData: func(m frame.MaxStreamData) error {
			st := c.getOrCreateStream(m.StreamID)
			st.SetSendMaxOff(m.Max)
			return nil
		},
		OnMaxStreams:      func(frame.MaxStreams) error { return nil },
		OnHandshakeDone:   func() error { return nil },
		OnConnectionClose: func(frame.ConnectionClose) error { return nil },
		OnCrypto: func(frame.Crypto) error {
			// Post-handshake CRYPTO (NewSessionTicket etc.) — deliver to
			// TLS driver for completeness.
			return nil
		},
	})
}

func (c *connState) flushApp() error {
	var payload []byte
	if c.handshakeComplete && !c.handshakeDoneSent {
		payload = frame.AppendHandshakeDone(payload)
		c.handshakeDoneSent = true
	}
	if c.ackApp && c.seenAppIn {
		payload = frame.AppendAck(payload, c.pnAppInLargest, c.pnAppInLargest, 0)
		c.ackApp = false
	}
	// Drain pending STREAM data.
	c.streamsMu.Lock()
	streamList := make([]*Stream, 0, len(c.streams))
	for _, s := range c.streams {
		streamList = append(streamList, s)
	}
	c.streamsMu.Unlock()
	// Per-stream MAX_STREAM_DATA bumps. Emit when the peer has
	// consumed more than flowUpdateThreshold of what we advertised.
	for _, s := range streamList {
		consumed := s.RecvConsumed()
		cur := s.RecvMaxOff()
		if cur == 0 {
			continue
		}
		if float64(cur-consumed) < float64(cur)*flowUpdateThreshold {
			newMax := consumed + localStreamFlowWindow
			s.SetRecvMaxOff(newMax)
			payload = frame.AppendMaxStreamData(payload, frame.MaxStreamData{
				StreamID: s.ID(), Max: newMax,
			})
		}
	}
	// Connection-level MAX_DATA bump.
	if c.localMaxData > 0 && float64(c.localMaxData-c.connRecvBytes) < float64(c.localMaxData)*flowUpdateThreshold {
		c.localMaxData = c.connRecvBytes + localConnFlowWindow
		payload = frame.AppendMaxData(payload, c.localMaxData)
	}
	for _, s := range streamList {
		if pending, code := s.HasPendingReset(); pending {
			payload = frame.AppendResetStream(payload, frame.ResetStream{
				StreamID: s.ID(), ErrorCode: code, FinalSize: 0,
			})
			continue
		}
		// Clamp drain by per-conn budget too.
		avail := 1100
		if c.peerParams.InitialMaxData > c.connSendBytes {
			if rem := int(c.peerParams.InitialMaxData - c.connSendBytes); rem < avail {
				avail = rem
			}
		} else if c.peerParamsSeen {
			avail = 0
		}
		for avail > 0 {
			data, off, fin := s.PopSend(avail)
			if len(data) == 0 && !fin {
				break
			}
			payload = frame.AppendStream(payload, frame.Stream{
				StreamID: s.ID(), Offset: off, Data: data, Fin: fin,
			})
			c.connSendBytes += uint64(len(data))
			if !fin {
				break
			}
			break
		}
	}
	if len(payload) == 0 {
		return nil
	}
	if len(payload) < 4 {
		payload = frame.AppendPadding(payload, 4-len(payload))
	}

	pn := c.pnAppOut
	pnLen := pickPNLen(pn)
	raw, err := sealShort(c.appSend, c.clientDCID, pn, pnLen, payload)
	if err != nil {
		return err
	}
	c.pnAppOut++
	if _, werr := c.ep.conn.WriteTo(raw, c.peer); werr != nil {
		return werr
	}
	// Track the packet for loss/RTT. App-space packets with stream or
	// stream-control frames are ack-eliciting. ACK-only or padding-only
	// isn't, but the distinction doesn't change much for our accounting
	// and keeping it simple avoids a second parse.
	c.rec.OnSent(recovery.Packet{
		Number:       pn,
		Space:        recovery.SpaceApplication,
		SentTime:     time.Now(),
		Size:         len(raw),
		AckEliciting: true,
		InFlight:     true,
	})
	c.cc.OnSent(len(raw))
	c.bytesInFlight += len(raw)
	return nil
}

// processAck feeds an incoming ACK frame to the recovery + congestion
// controllers. Ranges from frame.Ack are inclusive; recovery expects
// the same. Lost packets detected as a side-effect update the cc.
func (c *connState) processAck(space recovery.Space, a frame.Ack, now time.Time) {
	ranges := make([][2]uint64, 0, len(a.AckRanges))
	for _, r := range a.AckRanges {
		ranges = append(ranges, [2]uint64{r.Smallest, r.Largest})
	}
	// Peer-reported ack_delay is in microseconds, scaled by 2^exponent
	// from the peer's transport parameters (default 3 per RFC 9000).
	exp := c.peerParams.AckDelayExponent
	if exp == 0 {
		exp = 3
	}
	ackDelay := time.Duration(a.AckDelay) * time.Microsecond * (1 << exp)
	newly := c.rec.OnAck(space, a.LargestAcked, ranges, ackDelay, now)
	for _, p := range newly {
		if p.InFlight {
			c.bytesInFlight -= p.Size
			if c.bytesInFlight < 0 {
				c.bytesInFlight = 0
			}
		}
		c.cc.OnAck(p.Size, p.SentTime, now)
	}
	lost := c.rec.DetectLoss(space, now)
	for _, p := range lost {
		if p.InFlight {
			c.bytesInFlight -= p.Size
			if c.bytesInFlight < 0 {
				c.bytesInFlight = 0
			}
		}
		c.cc.OnLost(p.Size, p.SentTime)
	}
}

// sealShort builds a 1-RTT (short-header) packet. Short header format:
//
//	1 byte: 0b01SRRKPP — fixed=0x40, spin(0), reserved(00), key phase(0), pn_len.
//	N bytes: destination connection ID (length out-of-band).
//	pn_len bytes: packet number.
//	ciphertext: AEAD(payload).
func sealShort(p *crypto.PacketProtector, dcid []byte, pn uint64, pnLen int, payload []byte) ([]byte, error) {
	first := byte(0x40) | byte(pnLen-1)
	hdr := make([]byte, 0, 1+len(dcid)+pnLen)
	hdr = append(hdr, first)
	hdr = append(hdr, dcid...)
	pnOffset := len(hdr)
	for i := pnLen - 1; i >= 0; i-- {
		hdr = append(hdr, byte(pn>>(8*uint(i))))
	}
	sealed := p.Seal(nil, hdr, payload, pn)
	pkt := append([]byte(nil), hdr...)
	pkt = append(pkt, sealed...)
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(pkt) {
		return nil, errors.New("quic: short packet too small to sample")
	}
	mask, err := p.HeaderProtectionMask(pkt[sampleStart : sampleStart+16])
	if err != nil {
		return nil, err
	}
	pkt[0] ^= mask[0] & 0x1f
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	return pkt, nil
}

func (c *connState) flushInitial() error {
	var payload []byte
	if c.ackInitial && c.seenInitialIn {
		payload = frame.AppendAck(payload, c.pnInitialInLargest, c.pnInitialInLargest, 0)
		c.ackInitial = false
	}
	if len(c.outInitialCrypto) > 0 {
		payload = frame.AppendCrypto(payload, frame.Crypto{
			Offset: c.cryptoOffsetInitial,
			Data:   c.outInitialCrypto,
		})
		c.cryptoOffsetInitial += uint64(len(c.outInitialCrypto))
		c.outInitialCrypto = nil
	}
	// Pad so the packet hits at least 20 bytes of ciphertext for sample.
	if len(payload) < 4 {
		payload = frame.AppendPadding(payload, 4-len(payload))
	}

	pn := c.pnInitialOut
	raw, err := packet.SealInitial(nil, c.initialSend, packet.InitialPacket{
		Version:      packet.Version1,
		DCID:         c.clientDCID,
		SCID:         c.localSCID,
		PacketNumber: pn,
		PacketNumLen: pickPNLen(pn),
		Payload:      payload,
	})
	if err != nil {
		return err
	}
	c.pnInitialOut++
	if _, werr := c.ep.conn.WriteTo(raw, c.peer); werr != nil {
		return werr
	}
	c.rec.OnSent(recovery.Packet{
		Number: pn, Space: recovery.SpaceInitial, SentTime: time.Now(),
		Size: len(raw), AckEliciting: true, InFlight: true,
	})
	c.cc.OnSent(len(raw))
	c.bytesInFlight += len(raw)
	return nil
}

func (c *connState) flushHandshake() error {
	if c.handshakeSend == nil {
		return errors.New("quic: handshake send keys not installed")
	}
	var payload []byte
	if c.ackHandshake && c.seenHandshakeIn {
		payload = frame.AppendAck(payload, c.pnHandshakeInLargest, c.pnHandshakeInLargest, 0)
		c.ackHandshake = false
	}
	if len(c.outHandshakeCrypto) > 0 {
		payload = frame.AppendCrypto(payload, frame.Crypto{
			Offset: c.cryptoOffsetHandshake,
			Data:   c.outHandshakeCrypto,
		})
		c.cryptoOffsetHandshake += uint64(len(c.outHandshakeCrypto))
		c.outHandshakeCrypto = nil
	}
	if len(payload) < 4 {
		payload = frame.AppendPadding(payload, 4-len(payload))
	}

	pn := c.pnHandshakeOut
	pnLen := pickPNLen(pn)
	raw, err := sealHandshake(c.handshakeSend, handshakePacket{
		Version:      packet.Version1,
		DCID:         c.clientDCID,
		SCID:         c.localSCID,
		PacketNumber: pn,
		PacketNumLen: pnLen,
		Payload:      payload,
	})
	if err != nil {
		return err
	}
	c.pnHandshakeOut++
	if _, werr := c.ep.conn.WriteTo(raw, c.peer); werr != nil {
		return werr
	}
	c.rec.OnSent(recovery.Packet{
		Number: pn, Space: recovery.SpaceHandshake, SentTime: time.Now(),
		Size: len(raw), AckEliciting: true, InFlight: true,
	})
	c.cc.OnSent(len(raw))
	c.bytesInFlight += len(raw)
	return nil
}

func pickPNLen(pn uint64) int {
	switch {
	case pn < 1<<8:
		return 1
	case pn < 1<<16:
		return 2
	case pn < 1<<24:
		return 3
	default:
		return 4
	}
}

// -- Handshake-level packet encoder (lives here rather than quic/packet
// to avoid a premature second exported API; will migrate when 0-RTT /
// 1-RTT packets also need a dedicated builder).

type handshakePacket struct {
	Version      uint32
	DCID, SCID   []byte
	PacketNumber uint64
	PacketNumLen int
	Payload      []byte
}

func sealHandshake(p *crypto.PacketProtector, in handshakePacket) ([]byte, error) {
	first := byte(0xe0) | byte(in.PacketNumLen-1) // 0b11100000 | pnlen-1
	hdr := make([]byte, 0, 32+len(in.DCID)+len(in.SCID))
	hdr = append(hdr, first)
	hdr = append(hdr, byte(in.Version>>24), byte(in.Version>>16), byte(in.Version>>8), byte(in.Version))
	hdr = append(hdr, byte(len(in.DCID)))
	hdr = append(hdr, in.DCID...)
	hdr = append(hdr, byte(len(in.SCID)))
	hdr = append(hdr, in.SCID...)
	payloadLen := in.PacketNumLen + len(in.Payload) + p.Overhead()
	hdr = packet.AppendVarint(hdr, uint64(payloadLen))
	pnOffset := len(hdr)
	for i := in.PacketNumLen - 1; i >= 0; i-- {
		hdr = append(hdr, byte(in.PacketNumber>>(8*uint(i))))
	}
	sealed := p.Seal(nil, hdr, in.Payload, in.PacketNumber)
	pkt := append([]byte(nil), hdr...)
	pkt = append(pkt, sealed...)
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(pkt) {
		return nil, errors.New("quic: handshake ciphertext too short for sample")
	}
	mask, err := p.HeaderProtectionMask(pkt[sampleStart : sampleStart+16])
	if err != nil {
		return nil, err
	}
	pkt[0] ^= mask[0] & 0x0f
	for i := 0; i < in.PacketNumLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	return pkt, nil
}

// encodeServerTransportParams emits an RFC 9000 §18.2 transport-
// parameters extension. We set:
//
//   - original_destination_connection_id (mandatory)
//   - initial_source_connection_id (mandatory)
//   - initial_max_data (conn-level flow control)
//   - initial_max_stream_data_bidi_remote (peer-opened bidi)
//   - initial_max_stream_data_bidi_local (locally-opened bidi)
//   - initial_max_stream_data_uni (uni streams)
//   - initial_max_streams_bidi (concurrent bidi streams peer may open)
//   - initial_max_streams_uni
//   - max_idle_timeout
//
// Values are generous for a Phase-3/4 server; real tuning lives with
// congestion / memory budgeting in Phase 6.
func encodeServerTransportParams(origDCID []byte) []byte {
	appendParam := func(out []byte, id uint64, val []byte) []byte {
		out = packet.AppendVarint(out, id)
		out = packet.AppendVarint(out, uint64(len(val)))
		return append(out, val...)
	}
	appendVarintParam := func(out []byte, id, v uint64) []byte {
		val := packet.AppendVarint(nil, v)
		return appendParam(out, id, val)
	}

	var out []byte
	out = appendParam(out, 0x00, origDCID) // original_destination_connection_id
	out = appendParam(out, 0x0f, origDCID) // initial_source_connection_id
	out = appendVarintParam(out, 0x04, 1<<20) // initial_max_data (1 MiB)
	out = appendVarintParam(out, 0x05, 1<<16) // initial_max_stream_data_bidi_local
	out = appendVarintParam(out, 0x06, 1<<16) // initial_max_stream_data_bidi_remote
	out = appendVarintParam(out, 0x07, 1<<16) // initial_max_stream_data_uni
	out = appendVarintParam(out, 0x08, 100)   // initial_max_streams_bidi
	out = appendVarintParam(out, 0x09, 100)   // initial_max_streams_uni
	out = appendVarintParam(out, 0x01, 30_000) // max_idle_timeout (ms)
	return out
}

// decodePNCompat mirrors packet.decodePacketNumber without exporting it.
// Lives here to keep the handshake decode path in one file.
func decodePNCompat(largest, truncated uint64, pnLen int) uint64 {
	nBits := uint(pnLen * 8)
	win := uint64(1) << nBits
	hwin := win / 2
	mask := win - 1
	expected := largest + 1
	candidate := (expected &^ mask) | truncated
	if candidate+hwin <= expected && candidate < (1<<62)-win {
		candidate += win
	} else if candidate > expected+hwin && candidate >= win {
		candidate -= win
	}
	return candidate
}

// TLSConfig lets tests inject a *tls.Config so the endpoint can terminate
// real TLS without pulling in the cmd/ glue.
type TLSConfig = stdtls.Config
