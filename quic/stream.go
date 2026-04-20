package quic

import (
	"errors"
	"sync"
)

// Stream identifiers encode type (bidi/uni) and initiator (client/
// server) in the low two bits (RFC 9000 §2.1):
//
//	0x0: client-initiated, bidirectional
//	0x1: server-initiated, bidirectional
//	0x2: client-initiated, unidirectional
//	0x3: server-initiated, unidirectional
func StreamIsBidi(id uint64) bool          { return id&0x02 == 0 }
func StreamIsClientInit(id uint64) bool    { return id&0x01 == 0 }
func StreamIsServerInit(id uint64) bool    { return id&0x01 == 1 }

// Stream error codes carried in RESET_STREAM / STOP_SENDING.
const (
	StreamErrNoError       uint64 = 0x00
	StreamErrInternalError uint64 = 0x01
	StreamErrCancelled     uint64 = 0x10c // HTTP/3 application error (H3_REQUEST_CANCELLED)
)

// Stream is a single bidirectional byte stream on top of a QUIC
// connection. Methods are safe to call concurrently; the state machine
// is guarded by a mutex.
//
// The sending side maintains a write buffer that the connection pulls
// from when it wants to pack STREAM frames. The receiving side keeps
// an in-order byte queue populated by the connection's frame handler.
//
// Flow-control and congestion coordination is performed by the parent
// connection — the Stream only tracks its own offsets and FIN state.
type Stream struct {
	id uint64

	mu sync.Mutex

	// send side
	sendBuf      []byte // bytes queued but not yet written to the wire
	sendOffset   uint64 // next STREAM frame offset to hand the packer
	sendFin      bool   // application marked end-of-stream
	sendFinSent  bool   // FIN bit already flushed on the wire
	sendReset    bool   // RESET_STREAM sent
	sendResetCode uint64

	// receive side
	recvBuf      []byte // in-order, contiguous bytes delivered by the peer
	recvOffset   uint64 // next expected byte offset
	recvFin      bool
	recvFinObs   bool // peer signalled FIN and all bytes up to finOffset delivered
	recvFinOff   uint64
	recvReset    bool
	recvResetCode uint64

	// notifier, fires when recvBuf grows or state changes
	recvSignal chan struct{}
	closed     bool

	// Flow control (Phase 6).
	// sendMaxOff is the peer's latest advertised limit on our send
	// offset for this stream (from initial_max_stream_data_bidi_local
	// at init, grown by MAX_STREAM_DATA). PopSend refuses to cross it.
	// recvMaxOff is our advertised receive limit; OnStream drops any
	// bytes past it. We grow it by emitting MAX_STREAM_DATA when the
	// consumed window falls past half.
	sendMaxOff    uint64
	sendMaxOffSet bool
	recvMaxOff    uint64
	// recvConsumed counts bytes the application has read off the stream.
	// Used to decide when to send MAX_STREAM_DATA.
	recvConsumed uint64
}

// NewStream constructs a Stream with the given numeric identifier.
// Typically used by the connection's stream-table. The flow-control
// limits default to "unlimited" so tests that use Stream in isolation
// (without a parent connState) are unaffected; the connection resets
// them to the peer's and our advertised values at creation time.
func NewStream(id uint64) *Stream {
	return &Stream{id: id, recvSignal: make(chan struct{}, 1)}
}

// SetSendMaxOff installs the peer-advertised flow-control limit for
// this stream. Subsequent PopSend calls cap the drain at the limit.
// Called once at stream creation with the transport-param default,
// then on every inbound MAX_STREAM_DATA.
func (s *Stream) SetSendMaxOff(off uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sendMaxOffSet || off > s.sendMaxOff {
		s.sendMaxOff = off
	}
	s.sendMaxOffSet = true
}

// SetRecvMaxOff installs our advertised receive limit. Called at
// stream creation with the local initial_max_stream_data; the
// connection bumps it via MAX_STREAM_DATA as the app consumes data.
func (s *Stream) SetRecvMaxOff(off uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recvMaxOff = off
}

// RecvConsumed returns how many bytes the application has read from
// this stream. The connection's flush loop uses this + recvMaxOff to
// decide when to ship a MAX_STREAM_DATA.
func (s *Stream) RecvConsumed() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvConsumed
}

// RecvMaxOff returns our currently advertised receive limit.
func (s *Stream) RecvMaxOff() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvMaxOff
}

// ID returns the RFC 9000 stream identifier.
func (s *Stream) ID() uint64 { return s.id }

// Write appends bytes to the stream's send buffer. Returns n=len(b)
// and nil as long as the stream is open on the sending side.
func (s *Stream) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendFin || s.sendReset {
		return 0, errors.New("quic/stream: send side closed")
	}
	s.sendBuf = append(s.sendBuf, b...)
	return len(b), nil
}

// CloseWrite marks the send side as finished. The next STREAM frame
// the connection pulls will carry the FIN bit.
func (s *Stream) CloseWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendFin = true
	return nil
}

// ResetWrite abandons the send side with the given application error
// code. The connection packer will emit a RESET_STREAM on its next
// flush.
func (s *Stream) ResetWrite(code uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendReset = true
	s.sendResetCode = code
	s.sendBuf = nil
}

// PopSend drains up to max bytes from the send buffer and reports the
// offset they begin at, plus whether this drain should carry FIN.
// The caller (connection packer) is expected to wrap the result in a
// STREAM frame and hand it to the wire.
func (s *Stream) PopSend(max int) (data []byte, offset uint64, fin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sendBuf)
	if n > max {
		n = max
	}
	// Clamp to the peer's flow-control limit (when set).
	if s.sendMaxOffSet {
		if s.sendOffset >= s.sendMaxOff {
			n = 0
		} else if avail := s.sendMaxOff - s.sendOffset; uint64(n) > avail {
			n = int(avail)
		}
	}
	if n == 0 && !(s.sendFin && len(s.sendBuf) == 0 && !s.sendFinSent) {
		return nil, 0, false
	}
	data = s.sendBuf[:n:n]
	s.sendBuf = s.sendBuf[n:]
	offset = s.sendOffset
	s.sendOffset += uint64(n)
	if s.sendFin && len(s.sendBuf) == 0 && !s.sendFinSent {
		fin = true
		s.sendFinSent = true
	}
	return data, offset, fin
}

// HasPendingReset reports whether a RESET_STREAM should be emitted.
func (s *Stream) HasPendingReset() (bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendReset, s.sendResetCode
}

// OnStream delivers a received STREAM frame to the receive side. Out-
// of-order frames are dropped for now — a future revision will add a
// sparse reassembler. Duplicate frames (offset < recvOffset) are
// ignored but counted toward flow control.
func (s *Stream) OnStream(offset uint64, data []byte, fin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := offset + uint64(len(data))
	// Flow-control policing: refuse bytes past our advertised limit.
	// A strict peer wouldn't send them; a bug-or-hostile one gets
	// silently dropped (the connection-level enforcement in dispatch
	// reports FLOW_CONTROL_ERROR if truly abused).
	if s.recvMaxOff > 0 && end > s.recvMaxOff {
		return
	}
	switch {
	case offset == s.recvOffset:
		s.recvBuf = append(s.recvBuf, data...)
		s.recvOffset = end
	case end <= s.recvOffset:
		// Entirely before the receive window — pure duplicate.
	case offset < s.recvOffset && end > s.recvOffset:
		// Partial duplicate: take the tail.
		skip := s.recvOffset - offset
		s.recvBuf = append(s.recvBuf, data[skip:]...)
		s.recvOffset = end
	default:
		// offset > recvOffset: gap. Phase 3 scope is in-order streams,
		// drop for now.
		return
	}
	if fin {
		s.recvFin = true
		s.recvFinOff = end
		if s.recvOffset >= s.recvFinOff {
			s.recvFinObs = true
		}
	}
	s.signal()
}

// OnResetStream terminates the receive side with an application error.
func (s *Stream) OnResetStream(code, finalSize uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recvReset = true
	s.recvResetCode = code
	s.recvFinOff = finalSize
	s.recvFinObs = true
	s.signal()
}

// Read pulls up to len(p) bytes from the receive buffer. Returns io.EOF
// (via 0, nil at stream end — matching io.Reader is overkill for the
// proxy path, callers use the bytes-returned + fin flag directly).
func (s *Stream) Read(p []byte) (n int, fin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recvBuf) == 0 {
		return 0, s.recvFinObs
	}
	n = copy(p, s.recvBuf)
	s.recvBuf = s.recvBuf[n:]
	s.recvConsumed += uint64(n)
	if len(s.recvBuf) == 0 && s.recvFin && s.recvOffset >= s.recvFinOff {
		s.recvFinObs = true
	}
	fin = s.recvFinObs
	return n, fin
}

// RecvSignal returns a channel that receives a struct{} each time new
// data or state transitions become visible to Read. Allows callers to
// block without polling.
func (s *Stream) RecvSignal() <-chan struct{} { return s.recvSignal }

func (s *Stream) signal() {
	select {
	case s.recvSignal <- struct{}{}:
	default:
	}
}

// Close releases any pending signal receivers. Idempotent.
func (s *Stream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.recvSignal)
}
