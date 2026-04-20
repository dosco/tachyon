// Package quic is the transport layer for tachyon's HTTP/3 listener.
//
// The current implementation is the Phase 1 skeleton from
// /Users/vr/.claude/plans/add-http-3-and-quic-wondrous-starfish.md —
// a UDP read loop that parses packet headers, routes by destination
// connection ID, and responds to unsupported versions with a Version
// Negotiation packet. Handshake, streams, and HTTP/3 are not yet
// implemented; incoming Initial packets past version negotiation are
// logged and dropped.
package quic

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"tachyon/quic/packet"
)

// LocalConnIDLen is the length of the connection IDs tachyon chooses for
// itself. 8 bytes is the standard mid-range value; it gives each worker
// enough entropy to route short-header packets without blowing up per-
// packet overhead.
const LocalConnIDLen = 8

// Endpoint owns one UDP socket and demultiplexes incoming QUIC packets to
// per-connection state. Phase 1 has no connection objects yet, so it
// currently only records unique DCIDs seen and logs structural rejects.
type Endpoint struct {
	conn      net.PacketConn
	logger    *slog.Logger
	tlsConfig *tls.Config

	ctx context.Context

	mu    sync.RWMutex
	conns map[string]*connState // DCID → connection state

	packetsIn atomic.Uint64
	dropped   atomic.Uint64
}

// SetTLSConfig installs the TLS config used for terminating the
// handshake on accepted QUIC connections. NextProtos should include
// "h3". Call before Serve.
func (e *Endpoint) SetTLSConfig(c *tls.Config) { e.tlsConfig = c }

// randRead fills b with cryptographically random bytes, used to pick
// server connection IDs.
func (e *Endpoint) randRead(b []byte) (int, error) {
	return io.ReadFull(rand.Reader, b)
}

// NewEndpoint wraps an already-bound UDP socket. The caller typically
// obtains the conn from runtime.ListenPacket to pick up SO_REUSEPORT.
func NewEndpoint(conn net.PacketConn, logger *slog.Logger) *Endpoint {
	if logger == nil {
		logger = slog.Default()
	}
	return &Endpoint{
		conn:   conn,
		logger: logger,
		conns:  make(map[string]*connState),
	}
}

// Addr returns the bound local address.
func (e *Endpoint) Addr() net.Addr { return e.conn.LocalAddr() }

// Port returns the UDP port the endpoint is bound to as a string, for
// use in Alt-Svc advertisements. Returns "" if the local address
// cannot be parsed as a UDP address.
func (e *Endpoint) Port() string {
	a, ok := e.conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return strconv.Itoa(a.Port)
}

// Stats is a snapshot of endpoint counters.
type Stats struct {
	PacketsIn uint64
	Dropped   uint64
}

// Stats returns a snapshot of endpoint counters.
func (e *Endpoint) Stats() Stats {
	return Stats{
		PacketsIn: e.packetsIn.Load(),
		Dropped:   e.dropped.Load(),
	}
}

// Serve runs the UDP read loop until ctx is cancelled or the underlying
// socket is closed. It is safe to call Close concurrently with Serve.
func (e *Endpoint) Serve(ctx context.Context) error {
	// Max UDP payload in the wild is typically ~1500 bytes but GSO-
	// coalesced packets and jumbo MTUs can push it higher. 2048 is a
	// comfortable single-packet ceiling for now; Phase 6 will add a
	// reuse-pooled buffer with GRO support.
	buf := make([]byte, 2048)

	e.ctx = ctx
	// Propagate context cancellation by closing the socket; net.PacketConn
	// has no deadline-free cancel.
	go func() {
		<-ctx.Done()
		_ = e.conn.Close()
	}()

	for {
		n, addr, err := e.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		e.packetsIn.Add(1)
		e.handlePacket(buf[:n], addr)
	}
}

// Close releases the socket. Serve returns nil after Close.
func (e *Endpoint) Close() error { return e.conn.Close() }

func (e *Endpoint) handlePacket(b []byte, from net.Addr) {
	if len(b) == 0 {
		e.dropped.Add(1)
		return
	}

	// Short-header packets belong to an already-accepted connection; we
	// need our own DCID length to parse them, and for Phase 1 we only
	// surface a drop counter.
	if !packet.IsLongHeader(b[0]) {
		h, err := packet.Parse(b, LocalConnIDLen)
		if err != nil {
			e.dropped.Add(1)
			e.logger.Debug("quic: short-header parse failed", "err", err, "from", from)
			return
		}
		e.mu.RLock()
		cs, known := e.conns[string(h.DCID)]
		e.mu.RUnlock()
		if !known {
			e.dropped.Add(1)
			e.logger.Debug("quic: short header for unknown dcid", "dcid", h.DCID, "from", from)
			return
		}
		if err := cs.ReceiveShort(b); err != nil {
			e.dropped.Add(1)
			e.logger.Debug("quic: ReceiveShort failed", "err", err, "from", from)
			return
		}
		if err := cs.Flush(); err != nil {
			e.logger.Debug("quic: Flush after short failed", "err", err, "from", from)
		}
		return
	}

	h, err := packet.Parse(b, 0)
	switch {
	case errors.Is(err, packet.ErrVersionNeg):
		// Clients don't send VN packets; servers do. Drop.
		e.dropped.Add(1)
		return
	case err != nil:
		e.dropped.Add(1)
		e.logger.Debug("quic: long-header parse failed", "err", err, "from", from)
		return
	}

	if h.Version != packet.Version1 {
		e.sendVersionNegotiation(from, h.DCID, h.SCID)
		return
	}

	switch h.Type {
	case packet.LongInitial:
		e.handleInitial(b, h, from)
	case packet.LongHandshake:
		e.handleHandshake(b, h, from)
	default:
		// 0-RTT and Retry are out of scope for Phase 2.
		e.dropped.Add(1)
		e.logger.Debug("quic: unsupported long type", "type", int(h.Type), "from", from)
	}
}

func (e *Endpoint) handleInitial(b []byte, h packet.Header, from net.Addr) {
	key := string(h.DCID)
	e.mu.RLock()
	cs, ok := e.conns[key]
	e.mu.RUnlock()

	if !ok {
		if e.tlsConfig == nil {
			e.dropped.Add(1)
			e.logger.Debug("quic: Initial received but endpoint has no TLS config", "from", from)
			return
		}
		newCS, err := e.newServerConn(from, h.DCID, h.SCID)
		if err != nil {
			e.dropped.Add(1)
			e.logger.Debug("quic: newServerConn failed", "err", err, "from", from)
			return
		}
		if err := newCS.Start(e.ctx); err != nil {
			e.dropped.Add(1)
			e.logger.Debug("quic: tls.Start failed", "err", err, "from", from)
			return
		}
		e.mu.Lock()
		e.conns[key] = newCS
		// Also route by the SCID we chose, so subsequent client packets
		// addressed to us land on the same conn.
		e.conns[string(newCS.localSCID)] = newCS
		e.mu.Unlock()
		cs = newCS
	}

	if err := cs.ReceiveInitial(b); err != nil {
		e.dropped.Add(1)
		e.logger.Debug("quic: ReceiveInitial failed", "err", err, "from", from)
		return
	}
	if err := cs.Flush(); err != nil {
		e.logger.Debug("quic: Flush after Initial failed", "err", err, "from", from)
	}
}

func (e *Endpoint) handleHandshake(b []byte, h packet.Header, from net.Addr) {
	e.mu.RLock()
	cs, ok := e.conns[string(h.DCID)]
	e.mu.RUnlock()
	if !ok {
		e.dropped.Add(1)
		e.logger.Debug("quic: Handshake for unknown dcid", "from", from)
		return
	}
	if err := cs.ReceiveHandshake(b); err != nil {
		e.dropped.Add(1)
		e.logger.Debug("quic: ReceiveHandshake failed", "err", err, "from", from)
		return
	}
	if err := cs.Flush(); err != nil {
		e.logger.Debug("quic: Flush after Handshake failed", "err", err, "from", from)
	}
}

func (e *Endpoint) sendVersionNegotiation(to net.Addr, clientDCID, clientSCID []byte) {
	pkt := packet.BuildVersionNegotiation(nil, clientDCID, clientSCID, []uint32{packet.Version1})
	if _, err := e.conn.WriteTo(pkt, to); err != nil {
		e.logger.Debug("quic: version negotiation write failed", "err", err, "to", to)
	}
}
