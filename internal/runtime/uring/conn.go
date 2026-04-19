// Per-connection state for the io_uring event loop.
//
// Every accepted client lives in a slot of the worker's conn slab.
// Lookup cost is a slice index; "allocation" is a slot reuse after close.
// The state machine transitions are driven by CQE dispatch in server.go.

//go:build linux

package uring

import (
	"tachyon/http1"
)

type connState uint8

const (
	stFree connState = iota
	stReadingRequest        // waiting for client request headers
	stConnectingUp          // OP_CONNECT to upstream in flight (unused in v1: pre-dialed fds)
	stSendingUpRequest      // writing the proxied request to upstream (header block + any piggyback body)
	stSendingUpBody         // forwarding request body bytes to upstream (content-length or chunked)
	stReadingUpResponse     // waiting for upstream response headers
	stSendingClientResponse // writing the response back to the client
	stClosing               // close op in flight; drop on completion
)

// conn is the per-client state slab entry. Keep this struct cache-friendly;
// every worker iteration touches the active set.
type conn struct {
	state connState

	// Slot identity. seq is bumped on close so stale CQEs are ignored.
	seq uint32

	// File descriptors.
	clientFD int32
	upFD     int32
	poolName string // which fdPool upFD came from, for release

	// Read accumulator for client (request) and upstream (response).
	// Fixed 16 KiB slabs; parser consumes from the head.
	rdBuf     []byte // owned; len=cap=16384
	rdFilled  int    // bytes currently in rdBuf
	rdConsumed int   // bytes already consumed (header block size on parse success)

	upRdBuf    []byte
	upRdFilled int

	// Write buffers. We build the proxied request into wrBuf and the
	// proxied response into upWrBuf. Both are 4 KiB fixed; oversize
	// header blocks get 431.
	wrBuf   []byte
	wrLen   int // bytes queued to send to upstream
	wrSent  int // bytes acked by the upstream send CQE

	cliWrBuf  []byte
	cliWrLen  int
	cliWrSent int

	// Parsed request / response.
	req  http1.Request
	resp http1.Response

	// Body accounting for upstream-side body streaming.
	reqBodyRemaining int64 // request body bytes left to forward to upstream (Content-Length path)
	reqBodyChunked   bool  // request uses chunked transfer; forward until we see 0\r\n\r\n
	reqBodyDone      bool  // set when the chunked terminator has been observed
	reqBodyLen       int   // bytes in rdBuf queued for forwarding to upstream
	reqBodySent      int   // bytes of the queued chunk already sent to upstream

	respBodyRemaining int64 // response body bytes left to forward to client
	respBodyChunked   bool  // response uses chunked transfer; streams until EOF
	respBodyUntilEOF  bool  // HTTP/1.0 / Connection: close body

	// Keep-alive flags.
	closeAfter bool // drop client conn after this response

	// upBroken is set by error paths (send errno, recv EOF pre-parse,
	// parse error) so onSendClient's completion branch closes the
	// upstream instead of returning a known-bad socket to the pool.
	// Cleared on each new request (resetForKeepAlive). Kept even
	// after the P3b sticky-upstream experiment was reverted — this
	// marking is an independent correctness fix.
	upBroken bool

	// idleTicks counts consecutive 5-second reaper ticks during which
	// this conn has been in stReadingRequest with an empty rdBuf. At
	// 6 ticks (30 s) the reaper closes the slot (P3h). Reset on any
	// activity — recv, request dispatch, keep-alive reset.
	idleTicks uint8

	// spliceRemaining tracks the body bytes still to move through the
	// SPLICE chain for the current response (P3f). Set when we enter
	// the splice path in onRecvUp header-parse; decremented on each
	// opSpliceOut completion until 0.
	spliceRemaining int64
	// splicePipe is the per-conn pipe pair used for the SPLICE chain.
	// pipeRd is the read end (splice out -> client), pipeWr is the
	// write end (splice in <- upstream). We allocate one pair up-front
	// per slot; os.Pipe is not cheap enough to allocate per-body.
	pipeRd int32
	pipeWr int32
}

// resetForKeepAlive zeros the per-request state but keeps the slab
// buffers. The upstream fd is released by releaseOrCloseUpstream on
// the completion path before we get here, so upFD/poolName are
// normally already -1 / ""; the explicit zero is belt-and-suspenders
// for any path that forgot to release. Fires only on the keep-alive
// path; on close we go through freeSlot which tears everything down.
//
// Do NOT zero rdFilled / rdConsumed here — routeAndForward already
// called shiftRd to drop the consumed request from the head of rdBuf,
// leaving any pipelined-request bytes at rdBuf[0:rdFilled]. The
// onSendClient keep-alive path branches on rdFilled > 0 to parse
// those bytes via tryParseBuffered; zeroing them here would lose
// pipelined requests.
func (c *conn) resetForKeepAlive() {
	c.upRdFilled = 0
	c.wrLen = 0
	c.wrSent = 0
	c.cliWrLen = 0
	c.cliWrSent = 0
	c.reqBodyRemaining = 0
	c.reqBodyChunked = false
	c.reqBodyDone = false
	c.reqBodyLen = 0
	c.reqBodySent = 0
	c.respBodyRemaining = 0
	c.respBodyChunked = false
	c.respBodyUntilEOF = false
	c.closeAfter = false
	c.upBroken = false
	c.upFD = -1
	c.poolName = ""
	c.idleTicks = 0
	c.spliceRemaining = 0
	c.state = stReadingRequest
}

// shiftConsumed rolls the accumulator forward after a successful parse,
// preserving any leftover body bytes that arrived with the headers.
func (c *conn) shiftConsumed() {
	if c.rdConsumed == 0 {
		return
	}
	copy(c.rdBuf, c.rdBuf[c.rdConsumed:c.rdFilled])
	c.rdFilled -= c.rdConsumed
	c.rdConsumed = 0
}
