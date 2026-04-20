package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"tachyon/buf"
	"tachyon/http1"
	irt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
	"tachyon/internal/traffic"
	"tachyon/internal/upstream"
	"tachyon/metrics"
)

// Handler is the per-worker glue. It owns a router + pool set via an
// atomic pointer so SIGHUP reload can swap them without coordinating with
// in-flight requests: the next read of `.rp` sees the new value.
//
// Reads of the pointer are free relative to the rest of the request path
// (one atomic load on keep-alive loop entry), and reload happens at most
// on the order of once per operator action.
type Handler struct {
	rp atomic.Pointer[routerPools]
	is *irt.State

	// strictDeadlines, when true, re-arms read/write deadlines on every
	// request (both client and upstream) instead of the amortized
	// cadence. Operator escape hatch via -deadline-mode=perreq for
	// bisecting suspected deadline-related stalls. Default false.
	strictDeadlines bool

	// accessLog + accessLogger opt-in per-request debug logging. When
	// accessLog is false (the default), the hot path skips the logger
	// entirely — no slog.Record allocation, no handler call. When true,
	// each successful request emits one structured Debug line.
	accessLog    bool
	accessLogger *slog.Logger

	// altSvc, when non-empty, is appended as an Alt-Svc response header
	// on every client response. Set once at startup (before Serve) by
	// the QUIC bring-up code when an HTTP/3 listener is active.
	altSvc []byte
}

// SetAltSvc configures the Alt-Svc header value advertised on H1/H2
// responses (e.g. `h3=":8443"`). Empty value disables advertisement.
// Must be called before Serve begins.
func (h *Handler) SetAltSvc(v string) {
	if v == "" {
		h.altSvc = nil
		return
	}
	h.altSvc = []byte(v)
}

// AltSvc returns the configured Alt-Svc value; used by protocol
// handlers to inject the header into responses.
func (h *Handler) AltSvc() []byte { return h.altSvc }

// routerPools is the atomically-swappable bundle. Kept together so a
// reload installs a consistent pair: the new Router always references
// upstreams present in the new Pools.
type routerPools struct {
	r  *router.Router
	p  *upstream.Pools
	ip irt.RoutePrograms
}

// NewHandler builds a Handler with an initial router + pool set.
func NewHandler(r *router.Router, p *upstream.Pools, ip irt.RoutePrograms) *Handler {
	h := &Handler{is: irt.NewState()}
	h.rp.Store(&routerPools{r: r, p: p, ip: ip})
	return h
}

// SetStrictDeadlines toggles the per-request (true) vs. amortized
// (false, default) deadline-reset policy. Called at startup from the
// -deadline-mode flag; safe only before Serve begins.
func (h *Handler) SetStrictDeadlines(v bool) { h.strictDeadlines = v }

// SetAccessLog enables per-request structured logging at Debug level.
// log must be non-nil when enabled. Called once at startup from the
// -access-log flag; safe only before Serve begins.
func (h *Handler) SetAccessLog(log *slog.Logger) {
	h.accessLogger = log
	h.accessLog = log != nil
}

// Store atomically replaces the router + pool set. Called by SIGHUP reload.
// Callers are expected to close the previous pool set's idle conns after
// installing the new one.
func (h *Handler) Store(r *router.Router, p *upstream.Pools, ip irt.RoutePrograms) {
	h.rp.Store(&routerPools{r: r, p: p, ip: ip})
}

// Router returns the current router. Cheap atomic load.
func (h *Handler) Router() *router.Router { return h.rp.Load().r }

// Pools returns the current pool set. Cheap atomic load.
func (h *Handler) Pools() *upstream.Pools     { return h.rp.Load().p }
func (h *Handler) Intents() irt.RoutePrograms { return h.rp.Load().ip }

// ServeConn runs the per-client loop: parse request, route, forward,
// stream response, loop on keep-alive. Returns when the client closes or
// violates protocol.
//
// The hot path is single-syscall-per-direction for typical small-body
// requests:
//   - upstream write: one writev combining rewritten headers + any body
//     head bytes that piggybacked on the header read.
//   - client write: one write combining rewritten response headers + the
//     response body head (and for small bodies, the entire body).
//
// An upstream conn is acquired lazily on the first request and kept
// sticky across keep-alive requests that route to the same pool, so the
// hot loop never touches the pool mutex in steady state.
func (h *Handler) ServeConn(c net.Conn) {
	defer c.Close()

	rd := buf.Get(buf.ClassRead)
	wr := buf.Get(buf.ClassHeader)
	rs := buf.Get(buf.ClassRead)
	defer buf.Put(rd)
	defer buf.Put(wr)
	defer buf.Put(rs)

	// Raw TCPConn for splice when we need it on the body-copy path.
	clientTCP, _ := c.(*net.TCPConn)

	// Resolve the client IP once. RemoteAddr + String allocate; doing
	// this per-request was measurable in profiles. X-Forwarded-For
	// writers read this verbatim.
	clientAddr := clientIP(c)

	// Client deadline state. Bumped on the amortized cadence
	// (maybeBumpClientDeadline) — every DeadlineMaxUses requests or
	// every DeadlineRefresh wall-clock seconds, whichever comes first.
	// Slowloris defense is delegated to the 2-minute window itself;
	// TCP keep-alive on the dialer catches dead peers independently.
	//
	// Setting deadlines per-request showed up as ~7% of CPU in pprof —
	// Go's SetDeadline takes the poller lock and walks the fd's
	// operation list. For a small-body proxy that syscall cost is
	// larger than the work it guards. But setting it only ONCE at
	// Accept would EOF a long keep-alive conn at the 2-minute mark
	// (Phase 1 acceptance #3), so we amortize instead of eliding.
	var clientDeadlineUses uint32
	var clientDeadlineAt time.Time
	maybeBumpClientDeadline(c, &clientDeadlineUses, &clientDeadlineAt, h.strictDeadlines)

	var req http1.Request
	var resp http1.Response

	// Sticky upstream conn: acquired on first route match, reused across
	// every keep-alive request that targets the same pool. Released once
	// on ServeConn return.
	var stickyPool *upstream.Pool
	var stickyUC *upstream.Conn
	release := func() {
		if stickyUC != nil {
			stickyPool.Release(stickyUC)
			stickyUC = nil
			stickyPool = nil
		}
	}
	defer release()

	// Leading bytes that arrived with the previous request but belonged
	// to the next one (pipelined requests, or spurious trailing data).
	// The next readRequest call starts from this prefilled state instead
	// of waiting on a fresh Read.
	carry := 0

	for {
		// Re-arm the client deadline on the amortized cadence. Cheap on
		// 63 of every 64 iterations (one bool+uint+time-compare), does
		// the actual syscall on the 64th. This keeps the 2-minute
		// window rolling so long keep-alive clients don't hit EOF at
		// the 2-minute mark.
		maybeBumpClientDeadline(c, &clientDeadlineUses, &clientDeadlineAt, h.strictDeadlines)

		// Read until we have a complete header block. Bounded by the slab
		// size; oversized requests get 431. bodyHead is the slice of
		// already-read bytes that belong to the request body, or to the
		// next pipelined request (caller decides based on hasBody).
		bodyHead, carryOver, filled, err := readRequest(c, rd.Bytes(), carry, &req)
		if err != nil {
			sendError(c, err)
			return
		}
		// Record how far we touched rd so buf.Put zeros only that prefix
		// on return (Phase 2.B bounded zeroing). MarkWritten takes max,
		// so calling each iteration is safe and tracks the true
		// high-water across all iterations on this conn.
		rd.MarkWritten(filled)
		// Move any carry-over bytes to the start of rd for the next
		// iteration. carryOver is a slice into rd, so we copy before
		// the next Read overwrites.
		if len(carryOver) > 0 {
			copy(rd.Bytes(), carryOver)
			carry = len(carryOver)
		} else {
			carry = 0
		}

		// RFC 7231 §5.1.1: if the client asked "Expect: 100-continue",
		// answer it before we read/forward the body. We answer locally
		// and strip the Expect header from the forwarded request — that
		// keeps the upstream from sending a second 100, and lets us read
		// the body unconditionally.
		//
		// A client that sent the body eagerly without waiting for 100
		// still gets a 100 here; that's harmless per RFC and much simpler
		// than trying to detect "already got the body".
		if ev := req.Lookup(http1.HdrExpect); len(ev) > 0 &&
			http1.EqualFold(ev, http1.Value100Continue) {
			// Short write; ignore error — if the socket is dead the next
			// write will surface the failure and the loop exits cleanly.
			_, _ = c.Write(http1.Response100Continue)
		}

		// Route. Load router + pools once per keep-alive iteration; a
		// SIGHUP reload between iterations takes effect on the next
		// request without racing in-flight work.
		rp := h.rp.Load()
		host := string(req.Lookup(http1.HdrHost))
		match := rp.r.Match(host, req.PathBytes())
		if !match.Found {
			recordTraffic(&req, host, clientAddr, 0, 404, irt.Trace{RouteID: -1})
			sendStatus(c, 404)
			if req.Close {
				return
			}
			continue
		}
		upName := match.Upstream
		routeSet := rp.ip.ByRouteID[match.RouteID]
		reqView := h1IntentView{req: &req, host: host, path: string(req.PathBytes()), clientIP: clientAddr}
		var reqIntent irt.RequestResult
		if traffic.Enabled() {
			reqIntent = irt.ExecuteRequestTraced(routeSet, h.is, reqView, upName)
		} else {
			reqIntent = irt.ExecuteRequest(routeSet, h.is, reqView, upName)
		}
		if reqIntent.UpstreamOverride != "" {
			upName = reqIntent.UpstreamOverride
		}
		if reqIntent.HasTerminal {
			recordTraffic(&req, host, clientAddr, match.RouteID, reqIntent.Terminal.Status, reqIntent.Trace)
			sendTerminal(c, reqIntent.Terminal)
			if req.Close {
				return
			}
			continue
		}
		pool := rp.p.Get(upName)
		if pool == nil {
			recordTraffic(&req, host, clientAddr, match.RouteID, 502, reqIntent.Trace)
			sendStatus(c, 502)
			if req.Close {
				return
			}
			continue
		}

		// Acquire or reuse sticky upstream conn.
		if stickyUC == nil || stickyPool != pool {
			if stickyUC != nil {
				stickyPool.Release(stickyUC)
				stickyUC = nil
				stickyPool = nil
			}
			uc, err := pool.Acquire()
			if err != nil {
				metrics.Global.UpDialErr.Add(1)
				sendStatus(c, 502)
				if req.Close {
					return
				}
				continue
			}
			stickyUC = uc
			stickyPool = pool
		}

		// Forward. On upstream-level errors we drop the sticky conn (so
		// the next iteration re-acquires) and return 502 to the client
		// but keep the client conn open for the next request unless the
		// client asked for Connection: close.
		fwdStart := time.Now()
		fwdErr := forward(c, clientTCP, stickyUC, &req, &resp, bodyHead, wr, rs, clientAddr, h.strictDeadlines, reqIntent, routeSet, h.altSvc)
		if fwdErr != nil {
			// A malformed chunked body is a client-framing error; we
			// may have already written a partial body to upstream so
			// the upstream conn is unrecoverable. Break upstream and
			// return 400 (client error) rather than 502 (bad gateway).
			// We also force-close the client conn because a partial
			// body write means the client's next byte might be read
			// as a new request.
			malformed := errors.Is(fwdErr, http1.ErrMalformed)
			status := 502
			if malformed {
				status = 400
			}
			// Feed the outlier detector: a malformed client body is
			// NOT the upstream's fault, so only record gateway errors
			// for the genuine 502 path.
			if !malformed {
				stickyPool.RecordResult(stickyUC, 0, true, 0)
			}
			stickyUC.MarkBroken()
			stickyPool.Release(stickyUC)
			stickyUC = nil
			stickyPool = nil

			// One retry for idempotent methods when the budget allows.
			// Retries are only safe when: (a) the method has no request
			// body (GET/HEAD), and (b) nothing has been written to the
			// client yet — both are true here because we haven't called
			// sendStatus and GET/HEAD carry no body.
			if !malformed && pool.AllowRetry() && isIdempotent(req.MethodBytes()) {
				if retryUC, rerr := pool.Acquire(); rerr == nil {
					fwdStart = time.Now()
					retryErr := forward(c, clientTCP, retryUC, &req, &resp, bodyHead, wr, rs, clientAddr, h.strictDeadlines, reqIntent, routeSet, h.altSvc)
					if retryErr == nil {
						// Retry succeeded: account for the response and
						// let the normal post-forward path run.
						metrics.RecordStatus(int(resp.Status))
						pool.RecordResult(retryUC, int(resp.Status), false, uint64(time.Since(fwdStart)))
						if resp.Close {
							retryUC.MarkBroken()
							pool.Release(retryUC)
						} else {
							stickyUC = retryUC
							stickyPool = pool
						}
						if req.Close {
							return
						}
						continue
					}
					// Retry also failed; clean up and fall through to 502.
					pool.RecordResult(retryUC, 0, true, 0)
					retryUC.MarkBroken()
					pool.Release(retryUC)
				}
			}

			sendStatus(c, status)
			recordTraffic(&req, host, clientAddr, match.RouteID, status, reqIntent.Trace)
			if malformed || req.Close {
				return
			}
			continue
		}
		// Forward succeeded end-to-end. Count the upstream's status
		// class. This fires exactly once per successful request.
		metrics.RecordStatus(int(resp.Status))
		stickyPool.RecordResult(stickyUC, int(resp.Status), false, uint64(time.Since(fwdStart)))

		// Access log: only if operator opted in (-access-log) AND the
		// slog handler is actually listening at Debug level. Both
		// checks are needed: accessLog guards on a single bool load,
		// Enabled then guards on the logger's level so a non-Debug
		// handler doesn't pay the attribute-building cost.
		if h.accessLog && h.accessLogger.Enabled(nil, slog.LevelDebug) {
			h.accessLogger.Debug("access",
				"method", string(req.MethodBytes()),
				"path", string(req.PathBytes()),
				"host", host,
				"upstream", upName,
				"status", resp.Status,
				"peer", clientAddr,
			)
		}
		recordTraffic(&req, host, clientAddr, match.RouteID, int(resp.Status), reqIntent.Trace)

		// If the response said the upstream is closing, drop the sticky
		// conn so we dial fresh next time. Also honour Connection: close
		// from the response side: tell the client we're done too.
		if resp.Close {
			stickyUC.MarkBroken()
			stickyPool.Release(stickyUC)
			stickyUC = nil
			stickyPool = nil
		}

		if req.Close {
			return
		}
	}
}

// readRequest reads into rdBuf until Parse says the request is complete or
// ErrNeedMore says we need more bytes. `prefilled` is the count of bytes
// already at the head of rdBuf from a previous call (pipelined data). It
// returns:
//
//   - bodyHead: bytes past the header block that belong to the request
//     body (or to the next pipelined request, if the request has no body
//     — the caller decides by inspecting req.ContentLength / Chunked).
//   - carryOver: bytes in bodyHead that are NOT part of this request's
//     body. The caller moves these to the start of rdBuf before the next
//     iteration.
//   - filled: the total byte count touched in rdBuf. The caller uses
//     this to MarkWritten the slab so buf.Put's bounded clear zeros the
//     right prefix.
//
// The read deadline is set once per connection in ServeConn, not per
// request — SetReadDeadline is not free and shows up measurably in pprof.
func readRequest(c net.Conn, rdBuf []byte, prefilled int, req *http1.Request) ([]byte, []byte, int, error) {
	filled := prefilled
	// If we already have a complete request in the prefilled prefix, skip
	// the Read entirely.
	if filled > 0 {
		if n, perr := http1.Parse(rdBuf[:filled], req); perr == nil {
			bodyHead, carryOver := sliceBody(rdBuf, n, filled, req)
			return bodyHead, carryOver, filled, nil
		} else if !errors.Is(perr, http1.ErrNeedMore) {
			return nil, nil, filled, perr
		}
	}
	for {
		if filled == len(rdBuf) {
			return nil, nil, filled, http1.ErrTooLarge
		}
		nr, err := c.Read(rdBuf[filled:])
		if nr > 0 {
			filled += nr
			n, perr := http1.Parse(rdBuf[:filled], req)
			if perr == nil {
				bodyHead, carryOver := sliceBody(rdBuf, n, filled, req)
				return bodyHead, carryOver, filled, nil
			}
			if !errors.Is(perr, http1.ErrNeedMore) {
				return nil, nil, filled, perr
			}
			// Need more: keep reading.
		}
		if err != nil {
			if filled == 0 && errors.Is(err, io.EOF) {
				return nil, nil, filled, io.EOF
			}
			return nil, nil, filled, err
		}
	}
}

// sliceBody splits the bytes-after-headers into (bodyHead, carryOver)
// based on the parsed request's framing. Bytes that belong to this
// request's body are bodyHead; anything past that is a pipelined next
// request we carry to the next iteration.
func sliceBody(rdBuf []byte, headerEnd, filled int, req *http1.Request) ([]byte, []byte) {
	tail := rdBuf[headerEnd:filled]
	if req.ContentLength > 0 {
		n := int(req.ContentLength)
		if n > len(tail) {
			n = len(tail)
		}
		return tail[:n], tail[n:]
	}
	if req.Chunked {
		// We don't re-frame chunked bodies in this handler; we splice
		// bytes through until the client stops. Everything past the
		// headers goes to upstream as-is; no carry-over possible on
		// this request.
		return tail, nil
	}
	// No declared body. All trailing bytes belong to the next request.
	return nil, tail
}

// forward copies the parsed request to an upstream conn and copies the
// response back. bodyHead is any bytes that arrived with the header block
// and belong to the request body (possibly empty). wrBuf and rsBuf are
// reusable slabs owned by the caller — wrBuf holds the rewritten header
// block, rsBuf is the upstream-response read slab.
//
// Splitting header and body writes into two syscalls is what cheap proxies
// do and what the Phase 0 version did. This version:
//   - Packs the upstream request (headers + bodyHead + any available body)
//     into one write when it fits the 4 KiB wrBuf.
//   - Packs the client response the same way.
//   - Sets each direction's deadline once, not per Write.
//   - Falls back to `io.Copy` against the raw *net.TCPConn so the kernel
//     splice fast path fires for bigger bodies.
func forward(
	c net.Conn,
	clientTCP *net.TCPConn,
	uc *upstream.Conn,
	req *http1.Request,
	resp *http1.Response,
	bodyHead []byte,
	wrSlab, rsSlab *buf.Slab,
	clientAddr string,
	strictDeadlines bool,
	reqIntent irt.RequestResult,
	routeSet irt.RoutePolicySet,
	altSvc []byte,
) error {
	// --- Build upstream request ------------------------------------------

	wrBuf := wrSlab.Bytes()
	rsBuf := rsSlab.Bytes()
	w := wrBuf[:0]
	path := req.PathBytes()
	if reqIntent.PathOverride != "" {
		path = []byte(reqIntent.PathOverride)
	}
	w = http1.AppendRequestLine(w, req.MethodBytes(), path)

	// Append headers, skipping hop-by-hop and Host (we rewrite Host below).
	// Also skip Expect — we answered 100-continue locally; forwarding
	// Expect would make the upstream send a second 100.
	for i := 0; i < req.NumHeaders; i++ {
		src := req.Src()
		name := req.Headers[i].Name.Bytes(src)
		if isHopByHop(name) || http1.EqualFold(name, http1.HdrHost) ||
			http1.EqualFold(name, http1.HdrXForwardedFor) ||
			http1.EqualFold(name, http1.HdrExpect) ||
			intentHeaderRemoved(reqIntent.HeaderMutations, name) ||
			intentHeaderOverridden(reqIntent.HeaderMutations, name) {
			continue
		}
		w = http1.AppendHeader(w, name, req.Headers[i].Value.Bytes(src))
	}
	// Host -> upstream addr (simplest correct choice; real proxies allow
	// configured override).
	w = http1.AppendHeader(w, http1.HdrHost, []byte(uc.Addr))
	// Transfer-Encoding is hop-by-hop, so the header-forwarding loop
	// stripped it above. But we pass chunked bytes through verbatim —
	// the upstream still needs to know to parse them. Re-emit the
	// framing header so the next hop decodes the body correctly.
	// When Phase 2.C lands the validating ChunkedReader, this stays the
	// same shape; we'll just be re-emitting validated frames.
	if req.Chunked {
		w = http1.AppendHeader(w, http1.HdrTransferEncode, http1.ValueChunked)
	}
	// X-Forwarded-For: append client IP. clientAddr was resolved once at
	// ServeConn start; we don't re-parse RemoteAddr per request.
	if clientAddr != "" {
		if prev := req.Lookup(http1.HdrXForwardedFor); len(prev) > 0 {
			w = append(w, http1.HdrXForwardedFor...)
			w = append(w, ':', ' ')
			w = append(w, prev...)
			w = append(w, ',', ' ')
			w = append(w, clientAddr...)
			w = append(w, http1.CRLF...)
		} else {
			w = http1.AppendHeader(w, http1.HdrXForwardedFor, []byte(clientAddr))
		}
	}
	reqIntent.HeaderMutations.Each(func(hm irt.HeaderMutation) bool {
		if hm.Remove || hm.Name == "" {
			return true
		}
		w = http1.AppendHeader(w, []byte(hm.Name), []byte(hm.Value))
		return true
	})
	w = http1.AppendEndOfHeaders(w)

	// Pack bodyHead after the headers if it fits. One writev beats two
	// writes for the small-body case that dominates proxy traffic.
	// bodyHead is guaranteed to contain only this request's body bytes
	// (any pipelined trailing data was stripped by sliceBody).
	var bodyTail []byte
	if len(bodyHead) > 0 && len(w)+len(bodyHead) <= cap(wrBuf) {
		w = append(w, bodyHead...)
	} else if len(bodyHead) > 0 {
		bodyTail = bodyHead
	}

	// Record how much of wrSlab we touched building the upstream
	// request. MarkWritten takes max, so the later response-building
	// pass safely calls it again.
	wrSlab.MarkWritten(len(w))

	// Upstream deadline: amortized re-arm. Cheap comparison in the
	// common case; actual SetReadDeadline/SetWriteDeadline fires once
	// per DeadlineMaxUses writes or every DeadlineRefresh — which keeps
	// a long-lived sticky conn from lapsing past the 2-minute window.
	// strict mode forces a per-request bump for operator diagnosis.
	maybeBumpUpstreamDeadline(uc, strictDeadlines)
	if _, err := uc.Write(w); err != nil {
		metrics.Global.UpWriteErr.Add(1)
		return err
	}
	if len(bodyTail) > 0 {
		if _, err := uc.Write(bodyTail); err != nil {
			metrics.Global.UpWriteErr.Add(1)
			return err
		}
	}

	// Remaining request body (content-length or chunked). Splice when the
	// client is TCP — `*net.TCPConn.ReadFrom` special-cases LimitedReader
	// wrapping a *TCPConn and uses the kernel splice syscall.
	if req.ContentLength > 0 {
		remaining := req.ContentLength - int64(len(bodyHead))
		if remaining > 0 {
			if err := spliceN(uc, clientTCP, c, remaining); err != nil {
				return err
			}
		}
	} else if req.Chunked {
		// Validating chunked copy (Phase 2.C). The previous "splice
		// through" path trusted the client to produce well-formed
		// chunks. CopyChunkedBody now parses each chunk's size line
		// and trailing CRLF, rejecting malformed framing with
		// ErrMalformed — the caller turns that into a 400 and breaks
		// the upstream so we don't mid-stream a bad body.
		//
		// 2 KiB stack scratch is plenty: the sliding window only holds
		// a size line + CRLF + small residual between reads. Big chunk
		// payloads take the "remaining >= cap(scratch)" fast path
		// inside CopyChunkedBody and stream directly to dst.
		var chunkScratch [2048]byte
		if err := http1.CopyChunkedBody(uc, c, bodyHead, chunkScratch[:]); err != nil {
			return err
		}
	}

	// --- Read upstream response + stream back to client ------------------

	n, respBody, err := readResponse(uc, rsBuf, resp)
	if err != nil {
		metrics.Global.UpReadErr.Add(1)
		return err
	}
	// High-water mark in rsSlab = header bytes + body-piggyback.
	rsSlab.MarkWritten(n + len(respBody))

	respIntent := irt.ExecuteResponse(routeSet, func(name string) string {
		return string(resp.Lookup([]byte(name)))
	})
	w = wrBuf[:0]
	w = http1.AppendStatus(w, int(resp.Status))
	for i := 0; i < resp.NumHeaders; i++ {
		src := resp.Src()
		name := resp.Headers[i].Name.Bytes(src)
		if isHopByHop(name) || intentHeaderRemoved(respIntent.HeaderMutations, name) ||
			intentHeaderOverridden(respIntent.HeaderMutations, name) {
			continue
		}
		w = http1.AppendHeader(w, name, resp.Headers[i].Value.Bytes(src))
	}
	respIntent.HeaderMutations.Each(func(hm irt.HeaderMutation) bool {
		if hm.Remove || hm.Name == "" {
			return true
		}
		w = http1.AppendHeader(w, []byte(hm.Name), []byte(hm.Value))
		return true
	})
	if len(altSvc) > 0 {
		w = http1.AppendHeader(w, []byte("alt-svc"), altSvc)
	}
	w = http1.AppendEndOfHeaders(w)

	// Pack respBody after headers if it fits. Most 1 KiB responses land
	// entirely in respBody and we send everything in a single write.
	var respTail []byte
	if len(respBody) > 0 && len(w)+len(respBody) <= cap(wrBuf) {
		w = append(w, respBody...)
	} else if len(respBody) > 0 {
		respTail = respBody
	}

	// Record the response-build high-water into the same wrSlab. Takes
	// max against the earlier upstream-request len.
	wrSlab.MarkWritten(len(w))

	if _, err := c.Write(w); err != nil {
		return err
	}
	if len(respTail) > 0 {
		if _, err := c.Write(respTail); err != nil {
			return err
		}
	}

	// Remaining response body by framing. Splice dst and src when both
	// are TCPConns so no bytes traverse user space.
	switch {
	case resp.ContentLength > 0:
		remaining := resp.ContentLength - int64(len(respBody))
		if remaining > 0 {
			if err := spliceN(c, uc.TCP, uc, remaining); err != nil {
				return err
			}
		}
	case resp.Chunked:
		if err := spliceAll(c, uc.TCP, uc); err != nil {
			return err
		}
	case resp.Close:
		if err := spliceAll(c, uc.TCP, uc); err != nil {
			return err
		}
		uc.MarkBroken() // HTTP/1.0 style close; not reusable
	}

	return nil
}

// spliceN copies exactly n bytes from src to dst. If srcTCP is non-nil we
// hand `*net.TCPConn` to `io.CopyN` so Go's splice fast path fires: the
// kernel moves bytes between sockets without a user-space bounce buffer.
// Otherwise we fall back to the generic copy.
func spliceN(dst io.Writer, srcTCP *net.TCPConn, srcFallback io.Reader, n int64) error {
	if srcTCP != nil {
		_, err := io.CopyN(dst, srcTCP, n)
		return err
	}
	_, err := io.CopyN(dst, srcFallback, n)
	return err
}

// spliceAll is spliceN without a byte cap; used for chunked and
// Connection: close response bodies.
func spliceAll(dst io.Writer, srcTCP *net.TCPConn, srcFallback io.Reader) error {
	if srcTCP != nil {
		_, err := io.Copy(dst, srcTCP)
		return err
	}
	_, err := io.Copy(dst, srcFallback)
	return err
}

// Deadline amortization policy. 2-minute window on either direction;
// re-armed every DeadlineMaxUses writes OR every DeadlineRefresh since
// the last bump, whichever comes first. These numbers are chosen so:
//
//   - On the bench hot path (c=256, ~160k req/s/worker), the bump
//     fires once per ~64 requests per conn — roughly 500 extra
//     SetDeadline syscalls per second per worker. At ~200 ns each
//     that's 0.01% CPU, well under the 1% Phase-2 budget.
//   - On a slow keep-alive client (one request every 15s), the bump
//     fires after 30s via the time check, so the 2-minute window
//     never lapses on a cooperative peer. Plan acceptance #3
//     (keep-alive survives past 2 minutes) passes.
const (
	DeadlineWindow  = 2 * time.Minute
	DeadlineRefresh = 30 * time.Second
	DeadlineMaxUses = 64
)

// maybeBumpUpstreamDeadline re-arms the upstream conn's read+write
// deadlines on the amortized cadence above. Called once per request
// before the first upstream write. perRequest=true forces an unconditional
// bump (strict-mode operator override via -deadline-mode=perreq).
func maybeBumpUpstreamDeadline(uc *upstream.Conn, perRequest bool) {
	if !perRequest &&
		uc.DeadlineUses < DeadlineMaxUses &&
		!uc.DeadlineAt.IsZero() &&
		time.Since(uc.DeadlineAt) < DeadlineRefresh {
		uc.DeadlineUses++
		return
	}
	t := time.Now()
	_ = uc.SetReadDeadline(t.Add(DeadlineWindow))
	_ = uc.SetWriteDeadline(t.Add(DeadlineWindow))
	uc.DeadlineAt = t
	uc.DeadlineUses = 1
}

// maybeBumpClientDeadline re-arms the client conn's deadlines with the
// same cadence. ServeConn holds the counter+timestamp in locals because
// net.Conn doesn't expose a place to stash them.
func maybeBumpClientDeadline(c net.Conn, uses *uint32, lastAt *time.Time, perRequest bool) {
	if !perRequest &&
		*uses < DeadlineMaxUses &&
		!lastAt.IsZero() &&
		time.Since(*lastAt) < DeadlineRefresh {
		*uses++
		return
	}
	t := time.Now()
	_ = c.SetReadDeadline(t.Add(DeadlineWindow))
	_ = c.SetWriteDeadline(t.Add(DeadlineWindow))
	*lastAt = t
	*uses = 1
}

// readResponse mirrors readRequest but for a response. Returns the count of
// header bytes consumed and any body bytes that piggybacked on the header
// read.
func readResponse(uc net.Conn, rbuf []byte, resp *http1.Response) (int, []byte, error) {
	filled := 0
	for {
		if filled == len(rbuf) {
			return 0, nil, http1.ErrTooLarge
		}
		nr, err := uc.Read(rbuf[filled:])
		if nr > 0 {
			filled += nr
			n, perr := http1.ParseResponse(rbuf[:filled], resp)
			if perr == nil {
				return n, rbuf[n:filled], nil
			}
			if !errors.Is(perr, http1.ErrNeedMore) {
				return 0, nil, perr
			}
		}
		if err != nil {
			return 0, nil, err
		}
	}
}

// clientIP extracts the best-guess client IP for X-Forwarded-For.
func clientIP(c net.Conn) string {
	ra := c.RemoteAddr()
	if ra == nil {
		return ""
	}
	s := ra.String()
	// Strip ":port".
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i]
		}
	}
	return s
}

// sendStatus writes a minimal response with no body.
// isIdempotent reports whether method has no request body and can be
// safely retried without side-effects. We only consider GET and HEAD
// because OPTIONS/TRACE are rarely proxied and PUT/DELETE may have
// bodies that were already streamed to upstream on the first attempt.
func isIdempotent(method []byte) bool {
	return http1.EqualFold(method, http1.MethodGET) ||
		http1.EqualFold(method, http1.MethodHEAD)
}

func sendStatus(c net.Conn, code int) {
	var buf [128]byte
	w := http1.AppendStatus(buf[:0], code)
	w = http1.AppendContentLength(w, 0)
	w = http1.AppendEndOfHeaders(w)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(w)
	metrics.RecordStatus(code)
}

// sendError picks a status code based on err and writes it.
func sendError(c net.Conn, err error) {
	if errors.Is(err, io.EOF) {
		return
	}
	code := 400
	if errors.Is(err, http1.ErrTooLarge) {
		code = 431
	}
	sendStatus(c, code)
}
