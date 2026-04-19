// The H2 connection loop: preface → SETTINGS exchange → frame demux.
//
// Scope: server-only. One reader goroutine per connection drives frame
// demux; each request spawns its own goroutine for the handler so the
// reader can keep processing WINDOW_UPDATEs while respWriter.Write is
// parked on outbound flow-control credit. Frame emission is serialized
// through a single writeMu because frames on the wire must be atomic.
//
// Upstream forwarding is handled by a caller-supplied Handler. The
// Handler sees a decoded H1-style request (method/path/headers) and
// returns a status + header-field sequence + body reader.

//go:build linux

package http2

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"tachyon/http2/frame"
	"tachyon/http2/hpack"
)

// Handler processes a single decoded H2 request. Runs inline on the conn
// goroutine. Must be safe for concurrent calls if the conn is serving
// multiple streams interleaved (it is — DATA for stream N may arrive
// while we're still writing response frames for stream M).
type Handler interface {
	// ServeH2 dispatches a request. method/path/headers are valid only
	// for the duration of the call. body streams request DATA frames;
	// nil for requests without a body. The handler writes its response
	// via w. Return nil on success; errors are logged and the stream
	// is RST_STREAMed with INTERNAL_ERROR.
	ServeH2(method, path, authority string, headers []HeaderField, body io.Reader, w ResponseWriter) error
}

// HeaderField is a single decoded request header.
type HeaderField struct {
	Name, Value string
}

// ResponseWriter is the handler's interface to emit a response. Write
// order: WriteHeader once, then Write any number of times, then Close.
type ResponseWriter interface {
	WriteHeader(status int, fields []HeaderField) error
	Write(p []byte) (int, error)
	Close() error
}

// Serve runs the HTTP/2 server protocol on an already-established conn.
// Returns when the client disconnects or a protocol error occurs.
func Serve(nc net.Conn, h Handler) error {
	c := &conn{
		nc:      nc,
		br:      bufio.NewReaderSize(nc, 16*1024),
		bw:      bufio.NewWriterSize(nc, 16*1024),
		peer:    DefaultClient(),
		streams: make(map[uint32]*streamCtx, 32),
		h:       h,
		sendW:   NewWindow(int32(defaultInitialWindowSize)),
		recvW:   NewReceiveWindow(int32(defaultInitialWindowSize)),
		decDT:   hpack.NewDynamicTable(defaultHeaderTableSize),
		encDT:   hpack.NewDynamicTable(defaultHeaderTableSize),
	}
	c.dec = hpack.NewDecoder(c.decDT)
	c.enc = hpack.NewEncoder(c.encDT)
	c.flowCond = sync.NewCond(&c.flowMu)
	// Unblock any stream goroutines still parked in flowCond.Wait when the
	// conn tears down — otherwise they'd leak on error/EOF paths.
	defer func() {
		c.flowMu.Lock()
		c.closed = true
		c.flowMu.Unlock()
		c.flowCond.Broadcast()
		nc.Close()
	}()
	return c.run()
}

type conn struct {
	nc net.Conn
	br *bufio.Reader
	bw *bufio.Writer
	h  Handler

	peer    Settings
	streams map[uint32]*streamCtx

	// Connection-level flow windows.
	sendW Window
	recvW ReceiveWindow

	// HPACK state — one decoder (for incoming headers), one encoder (for
	// outgoing). Both share the same dynamic-table max-size negotiation
	// with the peer but use *separate* dynamic tables.
	decDT *hpack.DynamicTable
	encDT *hpack.DynamicTable
	dec   *hpack.Decoder
	enc   *hpack.Encoder

	// Header accumulation across HEADERS+CONTINUATION. Non-zero
	// pendingHeadersStream means we're between an initial HEADERS with
	// !END_HEADERS and the final CONTINUATION that carries END_HEADERS.
	// Exactly one stream may be mid-header per RFC 7540 §6.10.
	pendingHeadersStream uint32
	pendingBlock         []byte
	pendingEndStream     bool

	// Serialized writes: frames must be atomic on the wire. Also guards
	// hdrBuf, hdrEncBuf, and the HPACK encoder c.enc — any goroutine that
	// touches the encoder must hold writeMu.
	writeMu sync.Mutex

	// Workspace buffers reused by the write path. Both guarded by writeMu.
	hdrBuf    []byte // frame-assembly scratch
	hdrEncBuf []byte // HPACK block scratch (input to AppendHeaders)

	// Outbound flow-control coordination. flowMu guards sendW, every
	// live stream's SendWindow, every streamCtx.reset, and closed.
	// flowCond is signaled whenever credit becomes available or a stream
	// is reset, so parked respWriter goroutines can re-check their
	// windows.
	flowMu   sync.Mutex
	flowCond *sync.Cond
	closed   bool
}

type streamCtx struct {
	s      Stream
	bodyCh chan []byte // nil if no body expected; closed on END_STREAM
	done   chan struct{}
	// reset is set by onRSTStream and read by respWriter; guarded by
	// conn.flowMu. A stream-goroutine that wakes from flowCond.Wait
	// checks reset to bail out cleanly instead of continuing to push
	// DATA onto a stream the peer has abandoned.
	reset bool
}

func (c *conn) run() error {
	if err := c.readPreface(); err != nil {
		return err
	}
	// Send our SETTINGS + empty SETTINGS ACK after the client's first
	// SETTINGS arrives (handled inside the dispatch loop).
	if err := c.writeFrameFlush(func(b []byte) []byte {
		return frame.AppendSettings(b, false, Local())
	}); err != nil {
		return err
	}

	for {
		h, err := c.readFrameHeader()
		if err != nil {
			return err
		}
		if h.Length > defaultMaxFrameSize {
			return c.goAway(frame.ErrCodeFrameSizeError, "frame > max_frame_size")
		}
		payload := make([]byte, h.Length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return err
		}
		if err := c.dispatch(h, payload); err != nil {
			return err
		}
	}
}

func (c *conn) readPreface() error {
	var buf [PrefaceLen]byte
	if _, err := io.ReadFull(c.br, buf[:]); err != nil {
		return err
	}
	if string(buf[:]) != string(Preface) {
		return errors.New("http2: bad client preface")
	}
	return nil
}

func (c *conn) readFrameHeader() (frame.Header, error) {
	var buf [frame.HeaderSize]byte
	if _, err := io.ReadFull(c.br, buf[:]); err != nil {
		return frame.Header{}, err
	}
	return frame.ReadHeader(buf[:]), nil
}

// dispatch routes a parsed frame to the right handler. Returns an error
// only for fatal connection-level problems; stream-level errors result
// in RST_STREAM and continued service.
func (c *conn) dispatch(h frame.Header, payload []byte) error {
	// Header continuation enforcement: once we're mid-HEADERS, the only
	// legal follow-up is CONTINUATION on the same stream.
	if c.pendingHeadersStream != 0 {
		if h.Type != frame.TypeContinuation || h.StreamID != c.pendingHeadersStream {
			return c.goAway(frame.ErrCodeProtocolError, "expected CONTINUATION")
		}
	}
	switch h.Type {
	case frame.TypeSettings:
		return c.onSettings(h, payload)
	case frame.TypePing:
		return c.onPing(h, payload)
	case frame.TypeHeaders:
		return c.onHeaders(h, payload)
	case frame.TypeContinuation:
		return c.onContinuation(h, payload)
	case frame.TypeData:
		return c.onData(h, payload)
	case frame.TypeWindowUpdate:
		return c.onWindowUpdate(h, payload)
	case frame.TypeRSTStream:
		return c.onRSTStream(h, payload)
	case frame.TypePriority:
		_, err := frame.ReadPriority(payload)
		return err // ignore contents; parse-only
	case frame.TypeGoAway:
		// Peer is closing; drain streams and exit.
		return io.EOF
	case frame.TypePushPromise:
		return c.goAway(frame.ErrCodeProtocolError, "PUSH_PROMISE from client")
	default:
		// Unknown frame types MUST be ignored (§4.1).
		return nil
	}
}

func (c *conn) onSettings(h frame.Header, payload []byte) error {
	if h.Flags.Has(frame.FlagSettingsAck) {
		if len(payload) != 0 {
			return c.goAway(frame.ErrCodeFrameSizeError, "SETTINGS ACK with payload")
		}
		return nil
	}
	err := frame.ReadSettings(payload, func(s frame.Setting) bool {
		c.peer.Apply(s.ID, s.Value)
		return true
	})
	if err != nil {
		return c.goAway(frame.ErrCodeFrameSizeError, err.Error())
	}
	// Echo ACK.
	return c.writeFrameFlush(func(b []byte) []byte {
		return frame.AppendSettings(b, true, nil)
	})
}

func (c *conn) onPing(h frame.Header, payload []byte) error {
	p, ok := frame.ReadPing(h.Flags, payload)
	if !ok {
		return c.goAway(frame.ErrCodeFrameSizeError, "PING")
	}
	if p.Ack {
		return nil
	}
	return c.writeFrameFlush(func(b []byte) []byte {
		return frame.AppendPing(b, true, p.Data)
	})
}

func (c *conn) onHeaders(h frame.Header, payload []byte) error {
	hh, err := frame.ReadHeaders(h.Flags, payload)
	if err != nil {
		return c.goAway(frame.ErrCodeProtocolError, err.Error())
	}
	if !hh.EndHeaders {
		c.pendingHeadersStream = h.StreamID
		c.pendingBlock = append(c.pendingBlock[:0], hh.Block...)
		c.pendingEndStream = hh.EndStream
		return nil
	}
	return c.finalizeHeaders(h.StreamID, hh.Block, hh.EndStream)
}

func (c *conn) onContinuation(h frame.Header, payload []byte) error {
	c.pendingBlock = append(c.pendingBlock, payload...)
	if !h.Flags.Has(frame.FlagContinuationEndHdr) {
		return nil
	}
	sid := c.pendingHeadersStream
	c.pendingHeadersStream = 0
	block := c.pendingBlock
	c.pendingBlock = c.pendingBlock[:0]
	// We have to look up whether the original HEADERS carried END_STREAM;
	// simplification: we track it on pendingEndStream. Add a field next.
	return c.finalizeHeaders(sid, block, c.pendingEndStream)
}

// (Field split out so both the single-frame and CONTINUATION paths share it.)
func (c *conn) finalizeHeaders(streamID uint32, block []byte, endStream bool) error {
	var (
		method, path, authority, scheme string
		fields                          []HeaderField
		decodeErr                       error
	)
	err := c.dec.Decode(block, func(f hpack.Field) bool {
		name := string(f.Name)
		val := string(f.Value)
		switch name {
		case ":method":
			method = val
		case ":path":
			path = val
		case ":authority":
			authority = val
		case ":scheme":
			scheme = val
		case ":status":
			decodeErr = errors.New("http2: :status pseudo-header on request")
			return false
		default:
			if len(name) > 0 && name[0] == ':' {
				decodeErr = fmt.Errorf("http2: unknown pseudo-header %q", name)
				return false
			}
			fields = append(fields, HeaderField{Name: name, Value: val})
		}
		return true
	})
	if err != nil || decodeErr != nil {
		return c.goAway(frame.ErrCodeCompressionError, "hpack decode")
	}
	_ = scheme
	st := &streamCtx{done: make(chan struct{})}
	st.s.ID = streamID
	st.s.State = StateOpen
	st.s.RecvWindow = NewReceiveWindow(int32(defaultInitialWindowSize))
	st.s.SendWindow = int32(c.peer.InitialWindowSize)
	if endStream {
		st.s.State = StateHalfClosedRemote
		st.s.EndStreamSeen = true
	} else {
		st.bodyCh = make(chan []byte, 8)
	}
	c.streams[streamID] = st

	// Always dispatch the handler on its own goroutine. We used to inline
	// body-less requests, but that blocks the conn reader for the whole
	// upstream round-trip — including while the handler is waiting for
	// outbound flow-control credit. The only source of WINDOW_UPDATE
	// frames is this very reader, so blocking it deadlocks any response
	// larger than the peer's current send window. Running the handler on
	// a separate goroutine lets the reader keep dispatching frames while
	// respWriter.Write parks on flowCond.
	go c.runStream(st, method, path, authority, fields)
	return nil
}

func (c *conn) runStream(st *streamCtx, method, path, authority string, fields []HeaderField) {
	defer close(st.done)
	var body io.Reader
	if st.bodyCh != nil {
		body = &chanReader{ch: st.bodyCh}
	}
	w := &respWriter{c: c, st: st, streamID: st.s.ID}
	if err := c.h.ServeH2(method, path, authority, fields, body, w); err != nil {
		// Best-effort RST; ignore errors.
		_ = c.writeFrame(func(b []byte) []byte {
			return frame.AppendRSTStream(b, st.s.ID, frame.ErrCodeInternalError)
		})
	}
	_ = w.Close()
	// Flush batched frames for this stream's response.
	c.writeMu.Lock()
	_ = c.bw.Flush()
	c.writeMu.Unlock()
}

// chanReader adapts a channel of byte slices into an io.Reader.
type chanReader struct {
	ch  chan []byte
	buf []byte
}

func (r *chanReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		b, ok := <-r.ch
		if !ok {
			return 0, io.EOF
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// respWriter implements ResponseWriter by encoding HPACK and emitting
// HEADERS + DATA frames.
type respWriter struct {
	c        *conn
	st       *streamCtx
	streamID uint32
	wroteHdr bool
	closed   bool
}

func (w *respWriter) WriteHeader(status int, fields []HeaderField) error {
	if w.wroteHdr {
		return errors.New("http2: WriteHeader called twice")
	}
	w.wroteHdr = true
	// HPACK encoder state (dynamic table) is conn-scoped; serialize
	// access by building the block inside the writeMu-held callback.
	return w.c.writeFrame(func(b []byte) []byte {
		w.c.hdrEncBuf = w.c.enc.AppendIndexedStatus(w.c.hdrEncBuf[:0], status)
		for _, f := range fields {
			w.c.hdrEncBuf = w.c.enc.AppendField(w.c.hdrEncBuf, []byte(f.Name), []byte(f.Value))
		}
		return frame.AppendHeaders(b, w.streamID, false, true, w.c.hdrEncBuf)
	})
}

// errStreamReset / errConnClosed are returned from Write when the peer
// abandons the stream or the whole connection goes away mid-response.
var (
	errStreamReset = errors.New("http2: stream reset by peer")
	errConnClosed  = errors.New("http2: connection closed")
)

func (w *respWriter) Write(p []byte) (int, error) {
	if !w.wroteHdr {
		if err := w.WriteHeader(200, nil); err != nil {
			return 0, err
		}
	}
	// Per-frame cap negotiated via peer SETTINGS_MAX_FRAME_SIZE.
	maxFrame := int(w.c.peer.MaxFrameSize)
	if maxFrame == 0 {
		maxFrame = defaultMaxFrameSize
	}
	written := 0
	for len(p) > 0 {
		n, err := w.reserveCredit(len(p), maxFrame)
		if err != nil {
			return written, err
		}
		chunk := p[:n]
		if err := w.c.writeFrame(func(b []byte) []byte {
			return frame.AppendData(b, w.streamID, false, chunk)
		}); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// reserveCredit blocks until at least one byte of outbound DATA is
// admissible under both the connection and stream send windows, then
// deducts the reserved amount from both windows and returns it. The
// returned count is always > 0 unless an error is returned.
//
// The rules (RFC 7540 §5.2.1): a sender MUST NOT send more DATA bytes
// than the smaller of (conn send window, stream send window). We also
// cap the per-frame payload at peer SETTINGS_MAX_FRAME_SIZE.
func (w *respWriter) reserveCredit(want, maxFrame int) (int, error) {
	w.c.flowMu.Lock()
	defer w.c.flowMu.Unlock()
	for {
		if w.c.closed {
			return 0, errConnClosed
		}
		if w.st.reset {
			return 0, errStreamReset
		}
		cw := int(w.c.sendW.Remaining())
		sw := int(w.st.s.SendWindow)
		if cw > 0 && sw > 0 {
			n := want
			if n > maxFrame {
				n = maxFrame
			}
			if n > cw {
				n = cw
			}
			if n > sw {
				n = sw
			}
			// Reserved credit is committed here; the subsequent frame
			// write must happen (we treat a write error as fatal to
			// the conn, which is the caller's responsibility).
			w.c.sendW.Take(int32(n))
			w.st.s.SendWindow -= int32(n)
			return n, nil
		}
		w.c.flowCond.Wait()
	}
}

func (w *respWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if !w.wroteHdr {
		if err := w.WriteHeader(200, nil); err != nil {
			return err
		}
	}
	// Zero-length DATA with END_STREAM. Per RFC 7540 §6.9, flow control
	// applies to the DATA payload only — a zero-byte payload consumes no
	// window credit, so we can emit this without gating on flowCond even
	// if the stream/conn windows are exhausted.
	return w.c.writeFrame(func(b []byte) []byte {
		return frame.AppendData(b, w.streamID, true, nil)
	})
}

func (c *conn) onData(h frame.Header, payload []byte) error {
	d, err := frame.ReadData(h.Flags, payload)
	if err != nil {
		return c.goAway(frame.ErrCodeProtocolError, err.Error())
	}
	// Credit back at the connection level.
	if cr := c.recvW.Consume(int32(len(payload))); cr > 0 {
		if err := c.writeFrameFlush(func(b []byte) []byte {
			return frame.AppendWindowUpdate(b, 0, uint32(cr))
		}); err != nil {
			return err
		}
	}
	st := c.streams[h.StreamID]
	if st == nil {
		return c.writeFrameFlush(func(b []byte) []byte {
			return frame.AppendRSTStream(b, h.StreamID, frame.ErrCodeStreamClosed)
		})
	}
	if cr := st.s.RecvWindow.Consume(int32(len(payload))); cr > 0 {
		if err := c.writeFrameFlush(func(b []byte) []byte {
			return frame.AppendWindowUpdate(b, h.StreamID, uint32(cr))
		}); err != nil {
			return err
		}
	}
	if st.bodyCh != nil && len(d.Body) > 0 {
		cp := make([]byte, len(d.Body))
		copy(cp, d.Body)
		st.bodyCh <- cp
	}
	if d.EndStream && st.bodyCh != nil {
		close(st.bodyCh)
		st.bodyCh = nil
	}
	return nil
}

func (c *conn) onWindowUpdate(h frame.Header, payload []byte) error {
	inc, err := frame.ReadWindowUpdate(payload)
	if err != nil {
		return c.goAway(frame.ErrCodeProtocolError, err.Error())
	}
	if h.StreamID == 0 {
		c.flowMu.Lock()
		ok := c.sendW.Give(int32(inc))
		c.flowMu.Unlock()
		if !ok {
			return c.goAway(frame.ErrCodeFlowControlError, "conn window overflow")
		}
		c.flowCond.Broadcast()
		return nil
	}
	st := c.streams[h.StreamID]
	if st == nil {
		return nil
	}
	c.flowMu.Lock()
	overflow := st.s.SendWindow > 0x7fffffff-int32(inc)
	if !overflow {
		st.s.SendWindow += int32(inc)
	}
	c.flowMu.Unlock()
	if overflow {
		return c.writeFrameFlush(func(b []byte) []byte {
			return frame.AppendRSTStream(b, h.StreamID, frame.ErrCodeFlowControlError)
		})
	}
	c.flowCond.Broadcast()
	return nil
}

func (c *conn) onRSTStream(h frame.Header, payload []byte) error {
	_, err := frame.ReadRSTStream(payload)
	if err != nil {
		return c.goAway(frame.ErrCodeFrameSizeError, err.Error())
	}
	if st := c.streams[h.StreamID]; st != nil {
		st.s.State = StateClosed
		if st.bodyCh != nil {
			close(st.bodyCh)
			st.bodyCh = nil
		}
		// Flag the stream for its respWriter goroutine (which may be
		// parked in flowCond.Wait or about to enter reserveCredit) so
		// it aborts promptly instead of pushing more DATA onto a dead
		// stream.
		c.flowMu.Lock()
		st.reset = true
		c.flowMu.Unlock()
		c.flowCond.Broadcast()
		delete(c.streams, h.StreamID)
	}
	return nil
}

func (c *conn) goAway(code frame.ErrCode, msg string) error {
	_ = c.writeFrameFlush(func(b []byte) []byte {
		return frame.AppendGoAway(b, 0, code, []byte(msg))
	})
	return fmt.Errorf("http2: GOAWAY %d: %s", code, msg)
}

// writeFrame serializes one frame onto bw under writeMu. Flush is the
// caller's responsibility — batching is how we avoid a syscall per
// frame on multi-frame responses (HEADERS + DATA + END_STREAM = 1 flush).
func (c *conn) writeFrame(fill func([]byte) []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.hdrBuf = fill(c.hdrBuf[:0])
	_, err := c.bw.Write(c.hdrBuf)
	return err
}

// writeFrameFlush is for control frames that must reach the peer
// immediately (SETTINGS, PING ACK, WINDOW_UPDATE credit return, GOAWAY).
func (c *conn) writeFrameFlush(fill func([]byte) []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.hdrBuf = fill(c.hdrBuf[:0])
	if _, err := c.bw.Write(c.hdrBuf); err != nil {
		return err
	}
	return c.bw.Flush()
}

// helper: avoid "imported and not used" for binary on lean builds.
var _ = binary.BigEndian
var _ = strconv.Itoa
