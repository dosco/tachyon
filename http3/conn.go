// Package http3 is the HTTP/3 server on top of tachyon/quic.
//
// Scope:
//
//   - Request/response on client-initiated bidi streams.
//   - Unidirectional control stream exchange: the server opens a uni
//     stream with type 0x00 and sends a SETTINGS frame (RFC 9114
//     §6.2.1). It also accepts the peer's control stream plus QPACK
//     encoder (0x02) and decoder (0x03) streams.
//   - QPACK dynamic-table decoding. The server advertises
//     QPACK_MAX_TABLE_CAPACITY=4096 and QPACK_BLOCKED_STREAMS=16;
//     encoder-stream inserts update a per-connection decoder, field
//     sections can reference dynamic entries (request goroutines
//     block on Decoder.WaitForInsert when Required Insert Count runs
//     ahead of Known Received Count), Section Acknowledgment is
//     emitted on the decoder stream only for sections that actually
//     referenced dynamic entries, and Insert Count Increment tracks
//     drift back to the peer.
//
// Intentionally not implemented:
//
//   - Server push (type 0x01 PUSH_PROMISE, type 0x03 CANCEL_PUSH,
//     push streams). Chrome removed push support in M106 (2022);
//     Firefox never enabled it by default. RFC 9114 keeps it optional.
//   - QPACK dynamic-table insertions on the outgoing-response side.
//     Static-table references cover the headers a reverse proxy
//     typically returns; the win would be marginal and adds encoder
//     state we don't need.
//   - QUIC 0-RTT / early data. Needs request-layer replay-safety
//     policy (RFC 8470) plus a strike register. 1-RTT resumption IS
//     enabled; see cmd/tachyon/quic_tls.go.
//
// Each client-initiated bidirectional QUIC stream carries one HTTP/3
// request/response exchange. The wire layout is a sequence of HEADERS
// and DATA frames (RFC 9114 §4.1). HEADERS payloads are QPACK-encoded
// field sections.
package http3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"tachyon/http3/frame"
	"tachyon/http3/qpack"
	"tachyon/quic"
)

// Request is one parsed HTTP/3 request.
type Request struct {
	Method    string
	Scheme    string
	Authority string
	Path      string
	Headers   []qpack.Field // non-pseudo headers
	Body      []byte        // fully buffered for Phase 4; streaming comes with proxy integration
}

// ResponseWriter collects a response for a single HTTP/3 request. Call
// Write to append body bytes, then either Close() to FIN the stream or
// rely on the server to do so when the handler returns.
type ResponseWriter struct {
	Status int
	Header []qpack.Field
	Body   []byte
}

// Handler processes a single request. The server dispatches one Handler
// invocation per bidi stream.
type Handler func(context.Context, *Request, *ResponseWriter)

// Stream-type varints for unidirectional streams. RFC 9114 §6.2.
const (
	uniStreamControl      = 0x00
	uniStreamPush         = 0x01 // we reject these — push is disabled
	uniStreamQPACKEncoder = 0x02
	uniStreamQPACKDecoder = 0x03
)

// qpackDynamicCapacity is the QPACK_MAX_TABLE_CAPACITY value we
// advertise in SETTINGS. 4096 bytes is the industry-default point at
// which dynamic references start paying off on typical request header
// sets. MaxEntries = 4096/32 = 128 entries.
const qpackDynamicCapacity uint64 = 4096

// qpackBlockedStreams is the QPACK_BLOCKED_STREAMS value we advertise
// in SETTINGS. Advertising 0 tells encoders not to bother with the
// dynamic table — they'd have to wait for an ack before every
// reference — which Chrome and ngtcp2 observe by falling back to
// static-only encoding. 16 is the smallest value that lets real
// encoders keep their dynamic pipeline running; it also bounds the
// worst-case count of request goroutines parked in
// DecodeFieldSectionCtx on blocked sections.
const qpackBlockedStreams uint64 = 16

// connContext holds per-connection state for the HTTP/3 server: the
// QPACK decoder shared by all request streams, plus the server-side
// decoder stream that Section Acks flow out on.
type connContext struct {
	mu           sync.Mutex
	dec          *qpack.Decoder
	decStream    *quic.Stream // server-opened uni stream type 0x03
	pendingIncs  uint64       // accumulated Insert Count Increments owed to peer
	lastReported uint64       // insert count last reflected to peer
	flush        func() error
}

// newConnContext creates a fresh connContext with a decoder set to the
// advertised capacity.
func newConnContext(flush func() error) *connContext {
	return &connContext{
		dec:   qpack.NewDecoder(qpackDynamicCapacity),
		flush: flush,
	}
}

// handleEncoderBytes feeds bytes from the peer's encoder stream into
// the decoder, under the per-connection mutex. Any new inserts trigger
// a pending Insert Count Increment that will be flushed next time the
// decoder stream drains.
func (cc *connContext) handleEncoderBytes(b []byte) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	_, err := cc.dec.HandleEncoderStream(b)
	if err != nil {
		return err
	}
	ic := cc.dec.Table.InsertCount()
	if ic > cc.lastReported {
		cc.pendingIncs += ic - cc.lastReported
		cc.lastReported = ic
	}
	return cc.flushDecoderLocked()
}

// ackSection writes a Section Acknowledgment for streamID plus any
// pending Insert Count Increment, flushing the decoder stream.
func (cc *connContext) ackSection(streamID uint64) error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.decStream == nil {
		return nil
	}
	buf := qpack.EncodeSectionAck(nil, streamID)
	if cc.pendingIncs > 0 {
		buf = qpack.EncodeInsertCountIncrement(buf, cc.pendingIncs)
		cc.pendingIncs = 0
	}
	if _, err := cc.decStream.Write(buf); err != nil {
		return err
	}
	if cc.flush != nil {
		return cc.flush()
	}
	return nil
}

// flushPendingIncs drains any Insert Count Increment bookkeeping out
// to the decoder stream. Safe to call from the request-handling
// goroutine at a point where no stream-specific ack is needed.
func (cc *connContext) flushPendingIncs() error {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.flushDecoderLocked()
}

// flushDecoderLocked writes any pending Insert Count Increment. Caller
// holds cc.mu.
func (cc *connContext) flushDecoderLocked() error {
	if cc.decStream == nil || cc.pendingIncs == 0 {
		return nil
	}
	buf := qpack.EncodeInsertCountIncrement(nil, cc.pendingIncs)
	cc.pendingIncs = 0
	if _, err := cc.decStream.Write(buf); err != nil {
		return err
	}
	if cc.flush != nil {
		return cc.flush()
	}
	return nil
}

// Serve runs the HTTP/3 accept loop for cs, invoking h for each
// complete request it receives. On entry it opens the server control
// stream plus the QPACK decoder stream, and sends SETTINGS. Returns
// when ctx is done.
func Serve(ctx context.Context, cs Connection, h Handler) error {
	cc := newConnContext(cs.Flush)
	if err := openControlStream(cs); err != nil {
		return fmt.Errorf("http3: open control stream: %w", err)
	}
	if err := openDecoderStream(cs, cc); err != nil {
		return fmt.Errorf("http3: open decoder stream: %w", err)
	}
	for {
		s, err := cs.AcceptStream(ctx)
		if err != nil {
			return err
		}
		if quic.StreamIsBidi(s.ID()) {
			go serveStream(ctx, s, h, cc)
		} else {
			go serveUniStream(ctx, s, cc)
		}
	}
}

// Connection is the minimum surface http3 needs from a QUIC connection.
// An interface lets tests substitute an in-memory fake later.
type Connection interface {
	AcceptStream(context.Context) (*quic.Stream, error)
	OpenUniStream() (*quic.Stream, error)
	Flush() error
}

// ServeConn is a convenience wrapper that accepts a *quic.Conn and
// dispatches handlers. Identical to Serve but avoids making callers
// import the Connection interface name explicitly.
func ServeConn(ctx context.Context, c *quic.Conn, h Handler) error {
	return Serve(ctx, c, h)
}

// openControlStream opens a server-initiated unidirectional stream,
// writes the control-stream type prefix (0x00), then a SETTINGS frame.
func openControlStream(cs Connection) error {
	s, err := cs.OpenUniStream()
	if err != nil {
		return err
	}
	var out []byte
	out = append(out, byte(uniStreamControl))
	settings := frame.Settings{
		frame.SettingQPACKMaxTableCapacity: qpackDynamicCapacity,
		frame.SettingQPACKBlockedStreams:   qpackBlockedStreams,
		frame.SettingMaxFieldSectionSize:   65536,
	}
	out = frame.AppendSettings(out, settings)
	if _, err := s.Write(out); err != nil {
		return err
	}
	return cs.Flush()
}

// openDecoderStream opens a server-initiated unidirectional stream of
// type 0x03 (QPACK decoder stream) and records it on the connContext
// so later Section Acks and Insert Count Increments can flow.
func openDecoderStream(cs Connection, cc *connContext) error {
	s, err := cs.OpenUniStream()
	if err != nil {
		return err
	}
	if _, err := s.Write([]byte{byte(uniStreamQPACKDecoder)}); err != nil {
		return err
	}
	cc.mu.Lock()
	cc.decStream = s
	cc.mu.Unlock()
	return cs.Flush()
}

// serveUniStream consumes an incoming unidirectional stream. The first
// byte is the stream type: control and QPACK decoder streams are
// drained (we don't care about the peer's ACKs since we don't send
// dynamic inserts); the QPACK encoder stream is plumbed into the
// connection decoder so dynamic inserts become visible to request
// streams.
func serveUniStream(ctx context.Context, s *quic.Stream, cc *connContext) {
	defer s.Close()
	var prefix [1]byte
	var have int
	for have < 1 {
		n, fin := s.Read(prefix[have:])
		have += n
		if fin || have >= 1 {
			break
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.RecvSignal():
			}
		}
	}
	if have == 0 {
		return
	}
	streamType := uint64(prefix[0])
	switch streamType {
	case uniStreamQPACKEncoder:
		// Feed every read chunk straight into the per-connection
		// decoder. Partial instructions are buffered inside the
		// decoder's HandleEncoderStream tail.
		var carry []byte
		buf := make([]byte, 4096)
		for {
			n, fin := s.Read(buf)
			if n > 0 {
				chunk := append(carry, buf[:n]...)
				tail, err := cc.dec.HandleEncoderStream(chunk)
				if err != nil {
					return
				}
				carry = append(carry[:0], tail...)
				// Update pendingIncs under cc.mu without using
				// handleEncoderBytes (we already called
				// HandleEncoderStream directly to avoid re-lock/re-
				// parse). Recompute drift and flush.
				cc.mu.Lock()
				ic := cc.dec.Table.InsertCount()
				if ic > cc.lastReported {
					cc.pendingIncs += ic - cc.lastReported
					cc.lastReported = ic
				}
				_ = cc.flushDecoderLocked()
				cc.mu.Unlock()
			}
			if fin {
				return
			}
			if n == 0 {
				select {
				case <-ctx.Done():
					return
				case <-s.RecvSignal():
				}
			}
		}
	case uniStreamControl, uniStreamQPACKDecoder:
		// Drain silently. We don't depend on peer SETTINGS (our
		// encoder is static-only) or peer decoder-stream acks
		// (we don't emit dynamic inserts).
		buf := make([]byte, 4096)
		for {
			_, fin := s.Read(buf)
			if fin {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-s.RecvSignal():
			}
		}
	default:
		// Push (0x01) and unknown types: abort reading.
		return
	}
}

func serveStream(ctx context.Context, s *quic.Stream, h Handler, cc *connContext) {
	defer s.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	var fin bool
	for !fin {
		n, rfin := s.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if rfin {
			fin = true
			break
		}
		if n == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.RecvSignal():
			}
		}
	}

	req, usedDynamic, err := parseRequest(ctx, s.ID(), buf, cc)
	if err != nil {
		writeError(s, 400)
		return
	}
	// RFC 9204 §4.4.1: emit a Section Acknowledgment only if the
	// section actually referenced a dynamic entry. Unconditional acks
	// would corrupt the peer's Known Received Count bookkeeping.
	if usedDynamic {
		_ = cc.ackSection(s.ID())
	} else {
		// Still flush any pending Insert Count Increment that
		// accumulated while this request was decoding.
		_ = cc.flushPendingIncs()
	}

	rw := &ResponseWriter{Status: 200}
	h(ctx, req, rw)
	if err := writeResponse(s, rw); err != nil {
		return
	}
	_ = s.CloseWrite()
	if cc.flush != nil {
		_ = cc.flush()
	}
}

// parseRequest walks the HEADERS+DATA stream and materializes a
// Request. Trailers (a second HEADERS frame after DATA) are collected
// into Request.Headers alongside the leading ones. Uses the per-
// connection decoder so dynamic-table references resolve correctly.
// usedDynamic is true if any HEADERS frame on this stream referenced
// the dynamic table; the caller uses it to gate Section Ack.
func parseRequest(ctx context.Context, streamID uint64, buf []byte, cc *connContext) (*Request, bool, error) {
	r := &Request{}
	var usedDynamic bool
	for len(buf) > 0 {
		f, n, err := frame.Parse(buf)
		if err != nil {
			return nil, false, err
		}
		buf = buf[n:]
		switch f.Type {
		case frame.TypeHeaders:
			// Ctx variant waits (bounded) for encoder-stream inserts
			// when Required Insert Count runs ahead of Known Received
			// Count. Must be called without holding cc.mu, since the
			// encoder-stream reader takes cc.mu while processing
			// inserts that would unblock us.
			fields, _, used, err := cc.dec.DecodeFieldSectionCtx(ctx, streamID, f.Payload)
			if err != nil {
				return nil, false, err
			}
			if used {
				usedDynamic = true
			}
			for _, fd := range fields {
				switch fd.Name {
				case ":method":
					r.Method = fd.Value
				case ":scheme":
					r.Scheme = fd.Value
				case ":authority":
					r.Authority = fd.Value
				case ":path":
					r.Path = fd.Value
				default:
					r.Headers = append(r.Headers, fd)
				}
			}
		case frame.TypeData:
			r.Body = append(r.Body, f.Payload...)
		default:
			// Unknown frames on a request stream are legal per RFC 9114
			// §7.2.8 (reserved/grease); skip and continue.
		}
	}
	if r.Method == "" {
		return nil, false, errors.New("http3: request missing :method")
	}
	return r, usedDynamic, nil
}

func writeResponse(s *quic.Stream, rw *ResponseWriter) error {
	hdrs := []qpack.Field{{Name: ":status", Value: strconv.Itoa(rw.Status)}}
	hdrs = append(hdrs, rw.Header...)
	block := qpack.Encode(nil, hdrs)
	out := frame.AppendFrame(nil, frame.TypeHeaders, block)
	if len(rw.Body) > 0 {
		out = frame.AppendFrame(out, frame.TypeData, rw.Body)
	}
	if _, err := s.Write(out); err != nil {
		return err
	}
	return nil
}

func writeError(s *quic.Stream, status int) {
	rw := &ResponseWriter{Status: status, Body: []byte(fmt.Sprintf("status %d\n", status))}
	_ = writeResponse(s, rw)
	_ = s.CloseWrite()
}

// --- ResponseWriter helpers ---

// Write appends bytes to the response body.
func (rw *ResponseWriter) Write(p []byte) (int, error) {
	rw.Body = append(rw.Body, p...)
	return len(p), nil
}

// SetHeader appends a header field (case-sensitive name, per RFC 9113).
func (rw *ResponseWriter) SetHeader(name, value string) {
	rw.Header = append(rw.Header, qpack.Field{Name: name, Value: value})
}

var _ io.Writer = (*ResponseWriter)(nil)
