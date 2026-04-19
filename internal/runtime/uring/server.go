// The io_uring event loop.
//
// One goroutine owns one Ring, one PBUF_RING for client recvs, one conn
// slab, and one fd pool per upstream. It never blocks except inside
// io_uring_enter waiting for CQEs.

//go:build linux

package uring

import (
	"errors"
	"fmt"
	"log/slog"
	"syscall"

	"golang.org/x/sys/unix"

	"tachyon/http1"
	"tachyon/internal/router"
	"tachyon/iouring"
	"tachyon/iouring/buffers"
	"tachyon/iouring/op"
)

// UpstreamDef is the minimal info the worker needs to build its fd pool.
// Mirrors router.Upstream but kept here to keep the package's public
// surface tidy.
type UpstreamDef struct {
	Name        string
	Addrs       []string // first addr used; multi-addr LB is Phase 3
	IdlePerHost int
}

// Server is the event loop. One per worker process.
type Server struct {
	Router    *router.Router
	Upstreams []UpstreamDef
	Log       *slog.Logger

	// Tunables.
	RingEntries uint32 // default 4096
	PBufCount   uint16 // power of two; default 256
	PBufSize    int    // default 16384 (16 KiB; fits typical header block + first packet)
	MaxConns    uint32 // default 65536
	ReadSlab    int    // default 16384
	WriteSlab   int    // default 4096
	// SpliceMinBody is the response-body size threshold (bytes) above
	// which we switch from recv+copy+send to the zero-copy SPLICE chain
	// on plaintext conns (P3f). Default 16 KiB — below that, pipe
	// round-trip + two SPLICE SQEs cost more than the memcpy we'd save.
	// Set to 0 to disable splice entirely.
	SpliceMinBody int64

	// SQPoll, when true, sets IORING_SETUP_SQPOLL on the ring (P3g).
	// The kernel spawns a dedicated poller thread per ring that pulls
	// SQEs directly, so the hot path no longer needs io_uring_enter to
	// submit — a "zero-syscall steady state" at the cost of one kernel
	// thread per worker. Off by default; opt-in via -uring-sqpoll.
	//
	// Mutually exclusive with the IPI-related taskrun flags: the kernel
	// rejects SQPOLL combined with COOP_TASKRUN, TASKRUN_FLAG, or
	// DEFER_TASKRUN (EINVAL from io_uring_setup). With SQPOLL the
	// kernel's poller drains SQEs on its own, so userspace taskrun
	// notification is meaningless. Serve drops both CoopTaskrun and
	// DeferTaskrun when SQPoll is on.
	SQPoll bool

	// Set by Run.
	r      *iouring.Ring
	pbr    *buffers.Ring
	conns  []conn
	freeCh []uint32 // stack of free slot indices
	pools  map[string]*fdPool
	lfd    int // listening fd (raw)
}

// Serve binds addr with SO_REUSEPORT and runs the event loop.
func (s *Server) Serve(addr string) error {
	s.applyDefaults()

	lfd, err := ListenRaw(addr)
	if err != nil {
		return err
	}
	s.lfd = lfd
	defer unix.Close(lfd)

	// Build the ring. SingleIssuer is safe because the event loop
	// is the only submitter. Clamp avoids EINVAL on outsize entries.
	// SetupCoopTaskrun (5.19+): the kernel skips the IPI to wake our task
	// for completion work when we're already in userspace — we'll reap on
	// the next io_uring_enter anyway. SetupDeferTaskrun (6.1+): completion
	// work is deferred until we *explicitly* ask for it (SubmitAndWait or
	// enter-with-GETEVENTS), so CQEs never land mid-burst while we're
	// busy dispatching. Both require SingleIssuer, which we already have
	// because the event loop is the sole submitter.
	//
	// P3g: SQPOLL is mutually exclusive with all IPI-related taskrun
	// flags (COOP_TASKRUN, TASKRUN_FLAG, DEFER_TASKRUN) — the kernel
	// returns EINVAL from io_uring_setup. With SQPOLL the kernel thread
	// drains SQEs on its own, so userspace taskrun notification is
	// meaningless. When SQPoll is on we use Clamp+SingleIssuer+SQPoll
	// only; otherwise keep CoopTaskrun+DeferTaskrun.
	setupFlags := iouring.SetupClamp | iouring.SetupSingleIssuer
	if s.SQPoll {
		setupFlags |= iouring.SetupSQPoll
	} else {
		setupFlags |= iouring.SetupCoopTaskrun | iouring.SetupDeferTaskrun
	}
	r, err := iouring.New(s.RingEntries, setupFlags)
	if err != nil {
		return fmt.Errorf("uring: new: %w", err)
	}
	defer r.Close()
	s.r = r

	// Provided-buffer ring for client recvs.
	pbr, err := buffers.Provide(r.FD(), 1 /*bgID*/, s.PBufCount, s.PBufSize)
	if err != nil {
		return fmt.Errorf("uring: pbuf: %w", err)
	}
	defer pbr.Close()
	s.pbr = pbr

	// Upstream fd pools.
	s.pools = make(map[string]*fdPool, len(s.Upstreams))
	for _, u := range s.Upstreams {
		if len(u.Addrs) == 0 {
			continue
		}
		p, err := newFDPool(u.Addrs[0], u.IdlePerHost)
		if err != nil {
			return err
		}
		s.pools[u.Name] = p
	}

	// Conn slab + freelist.
	s.conns = make([]conn, s.MaxConns)
	s.freeCh = make([]uint32, 0, s.MaxConns)
	for i := uint32(s.MaxConns); i > 0; i-- {
		s.freeCh = append(s.freeCh, i-1)
	}
	// Per-conn buffers (rdBuf/upRdBuf/wrBuf/cliWrBuf) are lazy: allocSlot
	// makes them when the slot is claimed, freeSlot nils them for GC.
	// Eager allocation here pinned ~MaxConns × (2·ReadSlab+2·WriteSlab)
	// of heap regardless of the actual in-flight conn count.
	for i := range s.conns {
		s.conns[i].state = stFree
		s.conns[i].upFD = -1
		s.conns[i].pipeRd = -1
		s.conns[i].pipeWr = -1
	}

	// Arm multishot accept on the listener fd. This SQE stays live —
	// each accepted conn produces a CQE; on CQEFMore we re-arm if the
	// kernel tore it down.
	if err := s.armAccept(); err != nil {
		return err
	}
	// P3h: in-ring idle reaper. One TIMEOUT SQE re-armed every 5 s;
	// onTick walks the conn slab and closes any client conn stuck in
	// stReadingRequest for 30 s. No goroutine, no timer heap — the
	// kernel handles the sleep.
	s.armTick()
	if _, err := r.Submit(); err != nil {
		return fmt.Errorf("uring: initial submit: %w", err)
	}

	// Main loop: block for at least one CQE, dispatch every ready one.
	for {
		if _, err := r.SubmitAndWait(1); err != nil && !errors.Is(err, syscall.EINTR) {
			return fmt.Errorf("uring: submitwait: %w", err)
		}
		r.Drain(func(c *iouring.CQE) bool {
			s.dispatch(c)
			return true
		})
	}
}

// ---------------------------------------------------------------------
// Dispatch.
// ---------------------------------------------------------------------

func (s *Server) dispatch(c *iouring.CQE) {
	op, slot, seq := unpackUD(c.UserData)
	switch op {
	case opAcceptMulti:
		s.onAccept(c)
	case opRecvClient:
		s.onRecvClient(c, slot, seq)
	case opSendUp:
		s.onSendUp(c, slot, seq)
	case opRecvUp:
		s.onRecvUp(c, slot, seq)
	case opSendClient:
		s.onSendClient(c, slot, seq)
	case opSendUpBody:
		s.onSendUpBody(c, slot, seq)
	case opTick:
		s.onTick(c)
	case opSpliceIn:
		s.onSpliceIn(c, slot, seq)
	case opSpliceOut:
		s.onSpliceOut(c, slot, seq)
	case opCloseClient, opCloseUp:
		// Nothing to do; close CQE is purely informational.
	default:
		// Unknown tag — discard.
	}
}

// ---------------------------------------------------------------------
// Accept.
// ---------------------------------------------------------------------

func (s *Server) armAccept() error {
	sqe, err := s.r.Reserve()
	if err != nil {
		return err
	}
	op.AcceptMultishot(sqe, s.lfd, packUD(opAcceptMulti, 0, 0))
	return nil
}

func (s *Server) onAccept(c *iouring.CQE) {
	if c.Flags&iouring.CQEFMore == 0 {
		// Kernel tore the multishot down; re-arm.
		_ = s.armAccept()
	}
	if c.Res < 0 {
		return
	}
	fd := int(c.Res)
	// TCP_NODELAY so small replies flush. No SO_KEEPALIVE — we rely on
	// iouring timeouts for idle reaping (Phase 3).
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)

	slot, ok := s.allocSlot()
	if !ok {
		// Out of slots — drop.
		_ = unix.Close(fd)
		return
	}
	co := &s.conns[slot]
	co.clientFD = int32(fd)
	co.state = stReadingRequest
	s.armRecvClient(slot)
}

func (s *Server) allocSlot() (uint32, bool) {
	n := len(s.freeCh)
	if n == 0 {
		return 0, false
	}
	slot := s.freeCh[n-1]
	s.freeCh = s.freeCh[:n-1]
	co := &s.conns[slot]
	co.seq++ // make any straggler CQE stale
	co.rdBuf = make([]byte, s.ReadSlab)
	co.upRdBuf = make([]byte, s.ReadSlab)
	co.wrBuf = make([]byte, s.WriteSlab)
	co.cliWrBuf = make([]byte, s.WriteSlab)
	co.rdFilled = 0
	co.rdConsumed = 0
	co.upRdFilled = 0
	co.wrLen = 0
	co.wrSent = 0
	co.cliWrLen = 0
	co.cliWrSent = 0
	co.upFD = -1
	co.poolName = ""
	return slot, true
}

func (s *Server) freeSlot(slot uint32) {
	co := &s.conns[slot]
	co.state = stFree
	co.seq++
	if co.clientFD >= 0 {
		_ = unix.Close(int(co.clientFD))
		co.clientFD = -1
	}
	if co.upFD >= 0 {
		// Broken upstream fd — close it; don't return to pool.
		_ = unix.Close(int(co.upFD))
		co.upFD = -1
	}
	// P3f: release any lazy-allocated SPLICE pipe pair. Reusing pipes
	// across slots would be cheaper, but also opens up a cross-tenant
	// data-leak failure mode if we ever miscount splice bytes — close
	// on teardown and re-Pipe2 on next use.
	if co.pipeRd >= 0 {
		_ = unix.Close(int(co.pipeRd))
		co.pipeRd = -1
	}
	if co.pipeWr >= 0 {
		_ = unix.Close(int(co.pipeWr))
		co.pipeWr = -1
	}
	co.idleTicks = 0
	co.spliceRemaining = 0
	co.rdBuf = nil
	co.upRdBuf = nil
	co.wrBuf = nil
	co.cliWrBuf = nil
	s.freeCh = append(s.freeCh, slot)
}

// ---------------------------------------------------------------------
// Client recv (request).
// ---------------------------------------------------------------------

// armRecvClient arms a multishot recv against the PBUF_RING for this
// client conn. Fired once at accept and re-armed only when the kernel
// tears the multishot down (CQEFMore=0 on the CQE, typically because
// the buffer group briefly ran out). One SQE per conn for its lifetime —
// that's the P3a gain over one-shot RecvProvided: no per-request re-arm.
//
// The same recv funnels both header bytes (stReadingRequest) and request
// body bytes (stSendingUpBody) — onRecvClient dispatches by state.
// Mixing multishot recv with a second one-shot recv on the same fd would
// race, so the body path is unified here rather than having its own op.
func (s *Server) armRecvClient(slot uint32) {
	co := &s.conns[slot]
	sqe, err := s.r.Reserve()
	if err != nil {
		s.freeSlot(slot)
		return
	}
	op.RecvMultishot(sqe, int(co.clientFD), s.pbr.GroupID(), packUD(opRecvClient, slot, co.seq))
}

func (s *Server) onRecvClient(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]

	var (
		bid    uint16
		hasBuf = c.Flags&iouring.CQEFBuffer != 0
	)
	if hasBuf {
		bid = iouring.BufferID(c)
	}

	// Stale CQE for a recycled slot. Recycle the buffer; don't re-arm —
	// the slot's new occupant already armed its own multishot from
	// onAccept (if alive).
	if co.seq != seq {
		if hasBuf {
			s.pbr.Recycle(bid)
		}
		return
	}

	moreArmed := c.Flags&iouring.CQEFMore != 0

	if c.Res < 0 {
		if hasBuf {
			s.pbr.Recycle(bid)
		}
		// ENOBUFS with teardown: the PBUF_RING briefly ran out of free
		// buffers. Re-arm; we'll pick up traffic once the group refills
		// as other conns recycle.
		if c.Res == -int32(unix.ENOBUFS) && !moreArmed {
			s.armRecvClient(slot)
			return
		}
		s.freeSlot(slot)
		return
	}
	if c.Res == 0 {
		// Peer closed (FIN). No re-arm — we're tearing the slot down.
		if hasBuf {
			s.pbr.Recycle(bid)
		}
		s.freeSlot(slot)
		return
	}
	if !hasBuf {
		// Success without a provided buffer shouldn't happen with
		// buffer-select; bail defensively.
		s.freeSlot(slot)
		return
	}

	n := int(c.Res)
	data := s.pbr.Bytes(bid, n)
	overflow := co.rdFilled+n > len(co.rdBuf)
	if !overflow {
		copy(co.rdBuf[co.rdFilled:], data)
		co.rdFilled += n
	}
	s.pbr.Recycle(bid)

	// Kernel torn-down the multishot. Re-arm before dispatching so the
	// next packet isn't dropped.
	if !moreArmed {
		s.armRecvClient(slot)
	}

	if overflow {
		// In stReadingRequest a too-big header block is 431. In any
		// other state, rdBuf overflow means a pipelined-request or
		// body-stream that we can't hold; tear the slot down.
		if co.state == stReadingRequest {
			s.sendStatus(slot, 431)
		} else {
			s.freeSlot(slot)
		}
		return
	}

	switch co.state {
	case stReadingRequest:
		np, perr := http1.Parse(co.rdBuf[:co.rdFilled], &co.req)
		if perr == http1.ErrNeedMore {
			// Multishot is still armed; wait for more bytes.
			return
		}
		if perr != nil {
			s.sendStatus(slot, 400)
			return
		}
		co.rdConsumed = np
		// Expect: 100-continue — answer locally before forwarding.
		if ev := co.req.Lookup(http1.HdrExpect); len(ev) > 0 &&
			http1.EqualFold(ev, http1.Value100Continue) {
			_, _ = unix.Write(int(co.clientFD), http1.Response100Continue)
		}
		s.routeAndForward(slot)
	case stSendingUpBody:
		s.feedBodyBytes(slot, n)
	default:
		// stSendingUpRequest / stReadingUpResponse / stSendingClientResponse:
		// multishot landed pipelined-request bytes. They sit in rdBuf until
		// the current request finishes and keep-alive reset calls
		// tryParseBuffered.
	}
}

// ---------------------------------------------------------------------
// Upstream send (request).
// ---------------------------------------------------------------------

func (s *Server) armSendUp(slot uint32) {
	co := &s.conns[slot]
	sqe, err := s.r.Reserve()
	if err != nil {
		s.freeSlot(slot)
		return
	}
	op.Send(sqe, int(co.upFD), co.wrBuf[co.wrSent:co.wrLen], unix.MSG_NOSIGNAL, packUD(opSendUp, slot, co.seq))
}

func (s *Server) onSendUp(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]
	if co.seq != seq || co.state != stSendingUpRequest {
		return
	}
	if c.Res <= 0 {
		co.upBroken = true
		s.sendStatus(slot, 502)
		return
	}
	co.wrSent += int(c.Res)
	if co.wrSent < co.wrLen {
		s.armSendUp(slot)
		return
	}
	// Request header block (+ any piggybacked body) sent. If there's
	// more request body to forward, switch to body-streaming. With
	// multishot recv already armed on the client fd (P3a), body bytes
	// flow in through onRecvClient → feedBodyBytes. If the body-head
	// didn't fit in wrBuf it's still sitting at rdBuf[0:rdFilled];
	// kick a send off from those bytes now.
	if co.reqBodyRemaining > 0 || (co.reqBodyChunked && !co.reqBodyDone) {
		co.state = stSendingUpBody
		if co.rdFilled > 0 {
			s.feedBodyBytes(slot, co.rdFilled)
		}
		return
	}
	co.state = stReadingUpResponse
	co.upRdFilled = 0
	s.armRecvUp(slot)
}

// feedBodyBytes is the body-path analogue of what onRecvClientBody used
// to do: given n freshly-arrived bytes at the end of rdBuf (or, in the
// post-send-complete path, the entire rdBuf contents), decide how many
// belong to the request body, watch for the chunked terminator, and
// kick off an armSendUpBody.
//
// If a send for a previous chunk is still in flight (reqBodyLen > 0 and
// reqBodySent < reqBodyLen), we append to rdBuf and wait — the post-
// completion path in onSendUpBody will pick up any buffered bytes.
func (s *Server) feedBodyBytes(slot uint32, n int) {
	co := &s.conns[slot]
	if co.reqBodyLen > 0 && co.reqBodySent < co.reqBodyLen {
		return
	}
	take := n
	if co.reqBodyRemaining > 0 {
		if int64(take) > co.reqBodyRemaining {
			take = int(co.reqBodyRemaining)
		}
	}
	if co.reqBodyChunked {
		if containsChunkTerminator(co.rdBuf[co.rdFilled-n : co.rdFilled]) {
			co.reqBodyDone = true
		}
	}
	co.reqBodyLen = take
	co.reqBodySent = 0
	if co.reqBodyRemaining > 0 {
		co.reqBodyRemaining -= int64(take)
	}
	s.armSendUpBody(slot)
}

// armSendUpBody writes the queued body bytes to the upstream.
func (s *Server) armSendUpBody(slot uint32) {
	co := &s.conns[slot]
	sqe, err := s.r.Reserve()
	if err != nil {
		s.freeSlot(slot)
		return
	}
	// The body bytes are at the end of rdBuf: [rdFilled-reqBodyLen+reqBodySent : rdFilled].
	start := co.rdFilled - co.reqBodyLen + co.reqBodySent
	op.Send(sqe, int(co.upFD), co.rdBuf[start:co.rdFilled], unix.MSG_NOSIGNAL, packUD(opSendUpBody, slot, co.seq))
}

func (s *Server) onSendUpBody(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]
	if co.seq != seq || co.state != stSendingUpBody {
		return
	}
	if c.Res <= 0 {
		// Upstream write error mid-body: close upstream, 502.
		co.upBroken = true
		s.sendStatus(slot, 502)
		return
	}
	co.reqBodySent += int(c.Res)
	if co.reqBodySent < co.reqBodyLen {
		s.armSendUpBody(slot)
		return
	}
	// This chunk of body is fully forwarded. Reset the rdBuf tail so we
	// don't grow without bound across large bodies.
	co.rdFilled -= co.reqBodyLen
	co.reqBodyLen = 0
	co.reqBodySent = 0
	// Done?
	if co.reqBodyRemaining <= 0 && (!co.reqBodyChunked || co.reqBodyDone) {
		co.state = stReadingUpResponse
		co.upRdFilled = 0
		s.armRecvUp(slot)
		return
	}
	// Need more body bytes. With multishot recv armed on the client fd
	// (P3a), any bytes that arrived while we were sending the previous
	// chunk are already in rdBuf — pump another send from them. If rdBuf
	// is empty, sit idle; the next multishot CQE will call feedBodyBytes.
	if co.rdFilled > 0 {
		s.feedBodyBytes(slot, co.rdFilled)
	}
}

// ---------------------------------------------------------------------
// Upstream recv (response).
// ---------------------------------------------------------------------

func (s *Server) armRecvUp(slot uint32) {
	co := &s.conns[slot]
	sqe, err := s.r.Reserve()
	if err != nil {
		s.freeSlot(slot)
		return
	}
	op.Recv(sqe, int(co.upFD), co.upRdBuf[co.upRdFilled:], packUD(opRecvUp, slot, co.seq))
}

func (s *Server) onRecvUp(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]
	if co.seq != seq {
		return
	}
	if c.Res < 0 {
		co.upBroken = true
		s.sendStatus(slot, 502)
		return
	}
	if c.Res == 0 {
		// Upstream closed. If we're mid-body-until-EOF, flush. Else error.
		if co.state == stSendingClientResponse && co.respBodyUntilEOF {
			// Nothing more to send; done. Close client. Upstream already
			// closed — freeSlot's default-close path handles that.
			s.freeSlot(slot)
			return
		}
		// Peer-initiated EOF before/mid response → upstream is broken.
		co.upBroken = true
		s.sendStatus(slot, 502)
		return
	}

	if co.state == stReadingUpResponse {
		co.upRdFilled += int(c.Res)
		n, perr := http1.ParseResponse(co.upRdBuf[:co.upRdFilled], &co.resp)
		if perr == http1.ErrNeedMore {
			if co.upRdFilled == len(co.upRdBuf) {
				co.upBroken = true
				s.sendStatus(slot, 502)
				return
			}
			s.armRecvUp(slot)
			return
		}
		if perr != nil {
			co.upBroken = true
			s.sendStatus(slot, 502)
			return
		}
		// Build response headers into cliWrBuf.
		w := co.cliWrBuf[:0]
		w = http1.AppendStatus(w, int(co.resp.Status))
		src := co.resp.Src()
		for i := 0; i < co.resp.NumHeaders; i++ {
			name := co.resp.Headers[i].Name.Bytes(src)
			if isHopByHop(name) {
				continue
			}
			w = http1.AppendHeader(w, name, co.resp.Headers[i].Value.Bytes(src))
		}
		w = http1.AppendEndOfHeaders(w)
		// Any body bytes that arrived with the headers.
		bodyHead := co.upRdBuf[n:co.upRdFilled]
		if len(bodyHead) > 0 {
			w = append(w, bodyHead...)
		}
		if cap(co.cliWrBuf) < len(w) {
			co.cliWrBuf = make([]byte, len(w))
		}
		co.cliWrBuf = append(co.cliWrBuf[:0], w...)
		co.cliWrLen = len(w)
		co.cliWrSent = 0

		// Body accounting.
		co.respBodyChunked = co.resp.Chunked
		co.respBodyUntilEOF = co.resp.ContentLength < 0 && !co.resp.Chunked && co.resp.Close
		if co.resp.ContentLength > 0 {
			co.respBodyRemaining = co.resp.ContentLength - int64(len(bodyHead))
		} else {
			co.respBodyRemaining = 0
		}
		co.state = stSendingClientResponse
		s.armSendClient(slot)
		return
	}

	// State is stSendingClientResponse (streaming body). Forward what
	// we got.
	if co.state == stSendingClientResponse {
		// Use cliWrBuf as a transient body buffer.
		if cap(co.cliWrBuf) < int(c.Res) {
			co.cliWrBuf = make([]byte, c.Res)
		}
		co.cliWrBuf = append(co.cliWrBuf[:0], co.upRdBuf[:c.Res]...)
		co.cliWrLen = int(c.Res)
		co.cliWrSent = 0
		if co.respBodyRemaining > 0 {
			co.respBodyRemaining -= int64(c.Res)
		}
		s.armSendClient(slot)
	}
}

// ---------------------------------------------------------------------
// Client send (response).
// ---------------------------------------------------------------------

func (s *Server) armSendClient(slot uint32) {
	co := &s.conns[slot]
	sqe, err := s.r.Reserve()
	if err != nil {
		s.freeSlot(slot)
		return
	}
	op.Send(sqe, int(co.clientFD), co.cliWrBuf[co.cliWrSent:co.cliWrLen], unix.MSG_NOSIGNAL, packUD(opSendClient, slot, co.seq))
}

func (s *Server) onSendClient(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]
	if co.seq != seq {
		return
	}
	if c.Res <= 0 {
		s.freeSlot(slot)
		return
	}
	co.cliWrSent += int(c.Res)
	if co.cliWrSent < co.cliWrLen {
		s.armSendClient(slot)
		return
	}
	// Chunk done. If there's more body to forward, pick the forwarding
	// strategy:
	//
	//   - Plaintext + Content-Length body ≥ SpliceMinBody: zero-copy
	//     SPLICE chain (P3f). The header block is already on the client
	//     socket; the remaining body goes kernel-to-kernel via our
	//     per-conn pipe pair. No userspace copies.
	//
	//   - Else (chunked, close-delimited, small body, or pipe alloc
	//     failed): recv+copy+send — same as pre-P3f.
	if co.state == stSendingClientResponse && co.respBodyRemaining > 0 && !co.respBodyChunked && !co.respBodyUntilEOF &&
		s.SpliceMinBody > 0 && co.respBodyRemaining >= s.SpliceMinBody {
		if s.beginSpliceResponse(slot) {
			return
		}
		// Pipe alloc failed — fall through to the recv path.
	}
	if co.state == stSendingClientResponse && (co.respBodyRemaining > 0 || co.respBodyChunked || co.respBodyUntilEOF) {
		co.upRdFilled = 0
		s.armRecvUp(slot)
		return
	}
	// Response fully delivered. Release the upstream fd on every
	// completion — P3b's sticky-upstream experiment regressed keep/burst
	// by 2–5% on the 1-KiB GET bench (see the plan file's Phase 3 notes),
	// so we're back to the non-sticky shape. The upBroken flag still does
	// its job: error paths close the fd instead of returning a known-bad
	// socket to the pool, which is a correctness fix worth keeping.
	needClose := co.upBroken || co.respBodyChunked || co.respBodyUntilEOF || co.resp.Close
	s.releaseOrCloseUpstream(co, needClose)

	if co.closeAfter {
		s.freeSlot(slot)
		return
	}
	co.resetForKeepAlive()
	co.state = stReadingRequest
	// Multishot recv stays armed across keep-alive requests (P3a). If
	// rdBuf already has pipelined bytes from a previous CQE that landed
	// while we were still responding, try parsing them now. Otherwise
	// just sit — the next multishot CQE will drive onRecvClient.
	if co.rdFilled > 0 {
		s.tryParseBuffered(slot)
	}
}

// tryParseBuffered parses whatever is currently in rdBuf as a new
// request. If the parse is incomplete we return — the multishot recv
// already armed on this fd will deliver more bytes. Used only after
// keep-alive reset.
func (s *Server) tryParseBuffered(slot uint32) {
	co := &s.conns[slot]
	n, perr := http1.Parse(co.rdBuf[:co.rdFilled], &co.req)
	if perr == http1.ErrNeedMore {
		return
	}
	if perr != nil {
		s.sendStatus(slot, 400)
		return
	}
	co.rdConsumed = n
	if ev := co.req.Lookup(http1.HdrExpect); len(ev) > 0 &&
		http1.EqualFold(ev, http1.Value100Continue) {
		_, _ = unix.Write(int(co.clientFD), http1.Response100Continue)
	}
	s.routeAndForward(slot)
}

// routeAndForward is the post-parse branch of onRecvClient, factored out
// for the keep-alive reparse path.
func (s *Server) routeAndForward(slot uint32) {
	co := &s.conns[slot]
	host := string(co.req.Lookup(http1.HdrHost))
	upName := s.Router.Match(host, co.req.PathBytes())
	if upName == "" {
		s.sendStatus(slot, 404)
		return
	}
	pool := s.pools[upName]
	if pool == nil {
		s.sendStatus(slot, 502)
		return
	}

	// Acquire an upstream fd for this request. The onSendClient completion
	// path releases on every response, so co.upFD is always -1 here on the
	// normal keep-alive path; this guard covers the belt-and-suspenders
	// case of a prior error leaving one behind.
	if co.upFD >= 0 {
		s.releaseOrCloseUpstream(co, co.upBroken)
	}
	fd := pool.acquire()
	if fd < 0 {
		nfd, err := pool.dial()
		if err != nil {
			s.sendStatus(slot, 502)
			return
		}
		fd = nfd
	}
	co.upFD = int32(fd)
	co.poolName = upName

	w := co.wrBuf[:0]
	w = http1.AppendRequestLine(w, co.req.MethodBytes(), co.req.PathBytes())
	src := co.req.Src()
	for i := 0; i < co.req.NumHeaders; i++ {
		name := co.req.Headers[i].Name.Bytes(src)
		if isHopByHop(name) || http1.EqualFold(name, http1.HdrHost) ||
			http1.EqualFold(name, http1.HdrXForwardedFor) ||
			http1.EqualFold(name, http1.HdrExpect) {
			continue
		}
		w = http1.AppendHeader(w, name, co.req.Headers[i].Value.Bytes(src))
	}
	w = http1.AppendHeader(w, http1.HdrHost, []byte(pool.addr))
	w = http1.AppendEndOfHeaders(w)

	// Phase 1.F body forwarding. The H1 recv above read the header
	// block plus whatever body bytes happened to arrive in the same
	// packet; those live in rdBuf[rdConsumed:rdFilled] ("bodyHead").
	//
	// We pack bodyHead onto the end of the header write when it fits —
	// one syscall for header + first packet. Whatever doesn't fit, plus
	// any further body bytes, gets forwarded in stSendingUpBody.
	bodyHead := co.rdBuf[co.rdConsumed:co.rdFilled]
	co.reqBodyChunked = co.req.Chunked
	co.reqBodyDone = false
	if co.req.ContentLength > 0 {
		co.reqBodyRemaining = co.req.ContentLength
	} else {
		co.reqBodyRemaining = 0
	}

	// How much of bodyHead belongs to this request's body?
	bodyHeadForBody := 0
	if co.reqBodyRemaining > 0 {
		bodyHeadForBody = len(bodyHead)
		if int64(bodyHeadForBody) > co.reqBodyRemaining {
			bodyHeadForBody = int(co.reqBodyRemaining)
		}
	} else if co.reqBodyChunked {
		// For chunked, every post-header byte up to the terminator is
		// body. We don't parse here; we pass bytes through verbatim
		// and look for the "\r\n0\r\n\r\n" terminator along the way.
		bodyHeadForBody = len(bodyHead)
	}

	// Pack as much body as fits in the remainder of wrBuf.
	if bodyHeadForBody > 0 && len(w)+bodyHeadForBody <= cap(co.wrBuf) {
		w = append(w, bodyHead[:bodyHeadForBody]...)
		if co.reqBodyRemaining > 0 {
			co.reqBodyRemaining -= int64(bodyHeadForBody)
		}
		if co.reqBodyChunked && containsChunkTerminator(bodyHead[:bodyHeadForBody]) {
			co.reqBodyDone = true
		}
		// Consume these bytes from rdBuf so the next read starts clean.
		shiftRd(co, co.rdConsumed+bodyHeadForBody)
	} else {
		// bodyHead either doesn't exist or doesn't fit; forward headers
		// first, pick up the body in stSendingUpBody.
		shiftRd(co, co.rdConsumed)
	}

	if cap(co.wrBuf) < len(w) {
		co.wrBuf = make([]byte, len(w))
	}
	co.wrBuf = append(co.wrBuf[:0], w...)
	co.wrLen = len(w)
	co.wrSent = 0
	co.closeAfter = co.req.Close

	co.state = stSendingUpRequest
	s.armSendUp(slot)
}

// shiftRd consumes n bytes from the front of rdBuf, leaving any trailing
// data at the head for the next read/parse to pick up.
func shiftRd(co *conn, n int) {
	if n <= 0 {
		return
	}
	if n >= co.rdFilled {
		co.rdFilled = 0
		co.rdConsumed = 0
		return
	}
	copy(co.rdBuf, co.rdBuf[n:co.rdFilled])
	co.rdFilled -= n
	co.rdConsumed = 0
}

// containsChunkTerminator reports whether b ends with the chunked-encoding
// terminator. The canonical form is "0\r\n\r\n" (empty chunk followed by
// an empty trailer block). We accept anywhere in b because we scan after
// concatenated appends.
//
// This is deliberately simple: a fully correct chunked parser lives in
// Phase 2C. For Phase 1 correctness of POST/chunked we just need to
// recognize when the client has finished so we can switch to reading
// the response.
func containsChunkTerminator(b []byte) bool {
	// Look for "\n0\r\n\r\n" — the most common encoding. Also catch the
	// case where the buffer starts with "0\r\n\r\n" (a zero-length body
	// sent immediately after headers).
	term := []byte("\r\n0\r\n\r\n")
	if len(b) >= len(term) {
		for i := 0; i+len(term) <= len(b); i++ {
			match := true
			for j := 0; j < len(term); j++ {
				if b[i+j] != term[j] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	// Leading "0\r\n\r\n" (no preceding CRLF because bodyHead started
	// immediately past the header's trailing CRLF).
	lead := []byte("0\r\n\r\n")
	if len(b) >= len(lead) {
		match := true
		for j := 0; j < len(lead); j++ {
			if b[j] != lead[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Upstream release / close.
// ---------------------------------------------------------------------

// releaseOrCloseUpstream hands co.upFD back to its pool when `close` is
// false, or closes the fd when `close` is true (broken, or body framing
// left the conn in an ambiguous state). Always zeroes upFD/poolName
// after. Safe to call when upFD is already -1.
//
// P3b uses this on every transition that either tears down the slot or
// retargets the upstream. The old behaviour — release every response,
// re-acquire every request — lives only in the broken-path arm.
func (s *Server) releaseOrCloseUpstream(co *conn, closeIt bool) {
	if co.upFD < 0 {
		return
	}
	if !closeIt && co.poolName != "" {
		if p := s.pools[co.poolName]; p != nil {
			p.release(int(co.upFD))
			co.upFD = -1
			co.poolName = ""
			return
		}
	}
	_ = unix.Close(int(co.upFD))
	co.upFD = -1
	co.poolName = ""
}

// ---------------------------------------------------------------------
// Error responses.
// ---------------------------------------------------------------------

func (s *Server) sendStatus(slot uint32, code int) {
	co := &s.conns[slot]
	w := co.cliWrBuf[:0]
	w = http1.AppendStatus(w, code)
	w = http1.AppendContentLength(w, 0)
	w = http1.AppendEndOfHeaders(w)
	co.cliWrBuf = append(co.cliWrBuf[:0], w...)
	co.cliWrLen = len(w)
	co.cliWrSent = 0
	co.closeAfter = true
	co.respBodyRemaining = 0
	co.respBodyChunked = false
	co.respBodyUntilEOF = false
	co.state = stSendingClientResponse
	s.armSendClient(slot)
}

// ---------------------------------------------------------------------
// Defaults.
// ---------------------------------------------------------------------

func (s *Server) applyDefaults() {
	if s.RingEntries == 0 {
		s.RingEntries = 4096
	}
	if s.PBufCount == 0 {
		s.PBufCount = 256
	}
	if s.PBufSize == 0 {
		s.PBufSize = 16384
	}
	if s.MaxConns == 0 {
		s.MaxConns = 65536
	}
	if s.ReadSlab == 0 {
		s.ReadSlab = 16384
	}
	if s.WriteSlab == 0 {
		s.WriteSlab = 4096
	}
	// SpliceMinBody is 0-means-off here on purpose. The cmd-line flag
	// sets the "on by default" value (16 KiB) in main; leaving the
	// Server field zero — as tests and embedders do — means splice is
	// disabled and every body goes through the recv+send path.
}

// ---------------------------------------------------------------------
// P3h: in-ring idle reaper.
// ---------------------------------------------------------------------

// tickTS is the re-used 5-second Timespec for the reaper. One value for
// the whole server — Timeout's kernel-side semantics don't mutate it.
var tickTS = unix.Timespec{Sec: 5, Nsec: 0}

// tickIdleLimit is the number of consecutive ticks (tickTS apart) a conn
// may spend in stReadingRequest with an empty rdBuf before the reaper
// closes it. 6 ticks × 5 s = 30 s — same order of magnitude as the
// stdlib path's 30-second amortized deadline refresh.
const tickIdleLimit = 6

func (s *Server) armTick() {
	sqe, err := s.r.Reserve()
	if err != nil {
		return
	}
	// Relative timeout; Len=1 = fire on timeout only (we never want the
	// "after N CQEs" short-circuit semantics here).
	op.Timeout(sqe, &tickTS, 0, packUD(opTick, 0, 0))
}

// onTick walks the slab and closes conns that have been idle in
// stReadingRequest past tickIdleLimit ticks. Then it re-arms. Bounded
// work: O(MaxConns) per 5 s, which on MaxConns=65536 is a fast linear
// scan on the event-loop thread.
//
// We only consider stReadingRequest conns with no buffered bytes: any
// other state means activity is in flight (send CQE pending, body
// streaming, etc.) and the kernel will push the conn along.
func (s *Server) onTick(_ *iouring.CQE) {
	for i := range s.conns {
		co := &s.conns[i]
		if co.state != stReadingRequest || co.rdFilled > 0 {
			co.idleTicks = 0
			continue
		}
		co.idleTicks++
		if co.idleTicks >= tickIdleLimit {
			s.freeSlot(uint32(i))
		}
	}
	s.armTick()
}

// ---------------------------------------------------------------------
// P3f: SPLICE body forwarding (plaintext response body).
//
// Flow for a response body > SpliceMinBody on a plaintext conn:
//
//  1. After we've sent the response-header block on the client conn,
//     enter the splice chain instead of recv+copy+send.
//  2. SpliceIn: upstream_fd -> pipe_write. Up to 64 KiB per SQE.
//  3. SpliceOut: pipe_read  -> client_fd. Drains what SpliceIn pushed.
//  4. Repeat until spliceRemaining hits 0 or either side errors.
//
// The two SPLICEs are linked with IOSQE_IO_LINK so the kernel chains
// them without a user-space round-trip; the SpliceOut only runs if
// SpliceIn moved > 0 bytes.
//
// Correctness caveats:
//   - Plaintext only. TLS requires user-space encrypt, so kTLS or the
//     stdlib path handle that.
//   - Content-Length path only for now. Chunked response bodies stay on
//     the recv+copy+send path (framing lives in user space).
//   - On any SPLICE error we fall back to closing the conn — partial
//     writes are a framing hazard mid-response.
// ---------------------------------------------------------------------

// maxSplicePerSqe caps a single SpliceIn/Out to 64 KiB. Matches the
// default pipe buffer size on Linux — asking for more forces the kernel
// to chunk internally and we lose batching.
const maxSplicePerSqe uint32 = 64 * 1024

// beginSpliceResponse enters the splice chain after the response header
// block (including any body-head bytes) has already been queued for send
// on the client. Called from armSendClient's completion path when the
// remaining body exceeds SpliceMinBody.
//
// Returns false if pipe allocation fails — caller should stay on the
// recv+copy+send path.
func (s *Server) beginSpliceResponse(slot uint32) bool {
	co := &s.conns[slot]
	if co.pipeRd < 0 {
		var pfds [2]int
		// O_CLOEXEC: don't leak across exec. O_NONBLOCK: so a wedged
		// splice doesn't block the event loop thread.
		if err := unix.Pipe2(pfds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
			return false
		}
		co.pipeRd = int32(pfds[0])
		co.pipeWr = int32(pfds[1])
	}
	co.spliceRemaining = co.respBodyRemaining
	return s.armSpliceIn(slot)
}

// armSpliceIn reserves a SpliceIn SQE (upstream_fd -> pipe_write). Does
// NOT chain with SpliceOut here — SpliceOut is armed from onSpliceIn so
// we know how many bytes actually landed in the pipe.
func (s *Server) armSpliceIn(slot uint32) bool {
	co := &s.conns[slot]
	n := uint32(co.spliceRemaining)
	if int64(n) > int64(maxSplicePerSqe) {
		n = maxSplicePerSqe
	}
	sqe, err := s.r.Reserve()
	if err != nil {
		return false
	}
	op.Splice(sqe, int(co.upFD), -1, int(co.pipeWr), -1, n, 0, packUD(opSpliceIn, slot, co.seq))
	return true
}

func (s *Server) onSpliceIn(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]
	if co.seq != seq {
		return
	}
	if c.Res <= 0 {
		// Upstream errored / EOF mid-body. Drop the conn.
		co.upBroken = true
		s.freeSlot(slot)
		return
	}
	n := uint32(c.Res)
	sqe, err := s.r.Reserve()
	if err != nil {
		s.freeSlot(slot)
		return
	}
	op.Splice(sqe, int(co.pipeRd), -1, int(co.clientFD), -1, n, 0, packUD(opSpliceOut, slot, co.seq))
}

func (s *Server) onSpliceOut(c *iouring.CQE, slot, seq uint32) {
	co := &s.conns[slot]
	if co.seq != seq {
		return
	}
	if c.Res <= 0 {
		s.freeSlot(slot)
		return
	}
	co.spliceRemaining -= int64(c.Res)
	if co.spliceRemaining > 0 {
		if !s.armSpliceIn(slot) {
			s.freeSlot(slot)
		}
		return
	}
	// Body fully spliced. Release upstream (non-broken) and carry on with
	// the same post-response bookkeeping onSendClient does.
	co.respBodyRemaining = 0
	s.releaseOrCloseUpstream(co, co.upBroken || co.resp.Close)
	if co.closeAfter {
		s.freeSlot(slot)
		return
	}
	co.resetForKeepAlive()
	co.state = stReadingRequest
	if co.rdFilled > 0 {
		s.tryParseBuffered(slot)
	}
}
