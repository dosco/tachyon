// H2 handler: decoded H2 request → H1 upstream → H2 response frames.
//
// Phase 5b — drops net/http in favor of direct use of upstream.Pool and
// the tachyon http1 parser/writer the H1 handler uses. No client
// goroutine per upstream request; no heap allocation of *http.Request.
// The only per-call heap cost is the per-stream scratch buffers we pull
// from buf.Pool.

//go:build linux

package proxy

import (
	"io"
	"net"
	"time"

	"tachyon/buf"
	"tachyon/http1"
	"tachyon/http2"
	irt "tachyon/internal/intent/runtime"
)

// H2Handler adapts H2 requests into H1 upstream forwarding.
//
// It does not own router/pool state directly; it reads from the parent
// H1 Handler's atomic pointer so a SIGHUP reload propagates to both
// protocols in one write.
type H2Handler struct {
	parent *Handler
}

// NewH2Handler wires H2 to the same router + pools as the H1 handler it
// derives from. Reload happens via the parent's Store method.
func NewH2Handler(parent *Handler) *H2Handler {
	return &H2Handler{parent: parent}
}

// ServeH2 implements http2.Handler. Flow mirrors h1_handler.forward:
//
//  1. Build the upstream H1 request line + headers into a reused write
//     buffer, using the router-picked upstream's Addr as Host.
//  2. Write to upstream and stream the request body (if any) from body.
//  3. Parse the upstream response header block.
//  4. Emit a single HEADERS frame to the H2 peer.
//  5. Stream the response body as DATA frames.
func (h *H2Handler) ServeH2(method, path, authority string, fields []http2.HeaderField,
	body io.Reader, w http2.ResponseWriter,
) error {
	// Route. Prefer :authority, fall back to a Host field.
	host := authority
	if host == "" {
		for _, f := range fields {
			if http1.EqualFold([]byte(f.Name), http1.HdrHost) {
				host = f.Value
				break
			}
		}
	}
	rp := h.parent.rp.Load()
	match := rp.r.Match(host, []byte(path))
	if !match.Found {
		return w.WriteHeader(404, nil)
	}
	upName := match.Upstream
	routeSet := rp.ip.ByRouteID[match.RouteID]
	reqView := staticIntentView{
		method:   method,
		path:     path,
		host:     host,
		clientIP: "",
		fields:   toHeaderKVs(fields),
	}
	reqIntent := irt.ExecuteRequest(routeSet, h.parent.is, reqView, upName)
	if reqIntent.UpstreamOverride != "" {
		upName = reqIntent.UpstreamOverride
	}
	if reqIntent.HasTerminal {
		outFields := make([]http2.HeaderField, 0, reqIntent.Terminal.Headers.Len())
		reqIntent.Terminal.Headers.Each(func(hm irt.HeaderMutation) bool {
			if hm.Remove || hm.Name == "" {
				return true
			}
			outFields = append(outFields, http2.HeaderField{Name: lowerString([]byte(hm.Name)), Value: hm.Value})
			return true
		})
		if err := w.WriteHeader(reqIntent.Terminal.Status, outFields); err != nil {
			return err
		}
		if reqIntent.Terminal.Body != "" {
			_, err := io.WriteString(w, reqIntent.Terminal.Body)
			return err
		}
		return nil
	}
	pool := rp.p.Get(upName)
	if pool == nil {
		return w.WriteHeader(502, nil)
	}

	uc, err := pool.Acquire()
	if err != nil {
		return w.WriteHeader(502, nil)
	}
	// Release on every path; MarkBroken any time we had a write/read
	// failure so the pool closes rather than recycles.
	defer pool.Release(uc)

	// Per-request outlier-detection outcome. Set near the exit paths:
	// gwErr=true for any post-dial IO failure, recordedStatus=resp.Status
	// after a successful header parse. A deferred closure feeds both
	// back to the pool along with latency; RecordResult is a fast-return
	// no-op when neither outlier detection nor p2c stats are configured.
	var gwErr bool
	var recordedStatus int
	fwdStart := time.Now()
	defer func() {
		var lat uint64
		if !gwErr {
			lat = uint64(time.Since(fwdStart))
		}
		pool.RecordResult(uc, recordedStatus, gwErr, lat)
	}()

	// --- Write upstream request --------------------------------------

	wrSlab := buf.Get(buf.ClassHeader)
	defer buf.Put(wrSlab)
	wb := wrSlab.Bytes()[:0]

	if reqIntent.PathOverride != "" {
		path = reqIntent.PathOverride
	}
	wb = http1.AppendRequestLine(wb, []byte(method), []byte(path))
	hasBody := body != nil
	for _, f := range fields {
		name := []byte(f.Name)
		// Skip hop-by-hop and pseudo-headers (:method/:path/:authority/:scheme).
		if len(f.Name) > 0 && f.Name[0] == ':' {
			continue
		}
		if isHopByHop(name) || http1.EqualFold(name, http1.HdrHost) ||
			http1.EqualFold(name, http1.HdrExpect) {
			continue
		}
		if intentHeaderRemoved(reqIntent.HeaderMutations, name) ||
			intentHeaderOverridden(reqIntent.HeaderMutations, name) {
			continue
		}
		// When we reframe the body as HTTP/1.1 chunked below, we must
		// NOT also forward content-length — upstream would see two
		// conflicting framing signals. Drop it; the origin will read
		// the chunked body to its zero-size terminator.
		if hasBody && http1.EqualFold(name, http1.HdrContentLength) {
			continue
		}
		// And drop any Transfer-Encoding forwarded from H2 (shouldn't
		// appear — TE is HTTP/1-specific — but guard anyway).
		if http1.EqualFold(name, http1.HdrTransferEncode) {
			continue
		}
		wb = http1.AppendHeader(wb, name, []byte(f.Value))
	}
	wb = http1.AppendHeader(wb, http1.HdrHost, []byte(uc.Addr))
	reqIntent.HeaderMutations.Each(func(hm irt.HeaderMutation) bool {
		if hm.Remove || hm.Name == "" {
			return true
		}
		wb = http1.AppendHeader(wb, []byte(hm.Name), []byte(hm.Value))
		return true
	})
	// Phase 2.D: H2 DATA frames are framed by the H2 layer itself, so
	// by the time we get a body io.Reader here, length is unknown to
	// the H1 upstream. Inject Transfer-Encoding: chunked and reframe
	// below. Only for bodies we actually have — a pure GET with no
	// body MUST NOT declare chunked framing.
	if hasBody {
		wb = http1.AppendHeader(wb, http1.HdrTransferEncode, http1.ValueChunked)
	}
	wb = http1.AppendEndOfHeaders(wb)
	// Mark the header-build prefix so buf.Put's bounded clear zeros
	// only what we touched (Phase 2.B).
	wrSlab.MarkWritten(len(wb))

	_ = uc.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := uc.Write(wb); err != nil {
		gwErr = true
		uc.MarkBroken()
		return w.WriteHeader(502, nil)
	}
	// Reframe the H2 body as HTTP/1.1 chunked. Each Read from the H2
	// body becomes one chunk; EOF produces the last-chunk terminator.
	// We issue three writes per chunk (size-line, payload, CRLF)
	// instead of copying the payload into a scratch buffer — copy
	// elision beats syscall elision here because this path isn't on
	// the plaintext bench, but payloads can be sizable (H2 DATA
	// frames up to 16 KiB by default, up to max_frame_size by peer
	// settings).
	if hasBody {
		chSlab := buf.Get(buf.ClassBody)
		defer buf.Put(chSlab)
		chBuf := chSlab.Bytes()
		// Small scratch for the size-line framing. 20 bytes handles up
		// to 16 hex digits plus CRLF (covers int64 cap trivially).
		var sizeLine [20]byte
		crlf := http1.CRLF
		maxW := 0
		for {
			n, rerr := body.Read(chBuf)
			if n > 0 {
				sl := http1.AppendChunkSize(sizeLine[:0], n)
				if _, werr := uc.Write(sl); werr != nil {
					gwErr = true
					uc.MarkBroken()
					return w.WriteHeader(502, nil)
				}
				if _, werr := uc.Write(chBuf[:n]); werr != nil {
					gwErr = true
					uc.MarkBroken()
					return w.WriteHeader(502, nil)
				}
				if _, werr := uc.Write(crlf); werr != nil {
					gwErr = true
					uc.MarkBroken()
					return w.WriteHeader(502, nil)
				}
				if n > maxW {
					maxW = n
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				gwErr = true
				uc.MarkBroken()
				return w.WriteHeader(502, nil)
			}
		}
		// Terminator.
		var tail [8]byte
		last := http1.LastChunk(tail[:0])
		if _, err := uc.Write(last); err != nil {
			gwErr = true
			uc.MarkBroken()
			return w.WriteHeader(502, nil)
		}
		chSlab.MarkWritten(maxW)
	}

	// --- Read upstream response --------------------------------------

	_ = uc.SetReadDeadline(time.Now().Add(30 * time.Second))
	rdSlab := buf.Get(buf.ClassRead)
	defer buf.Put(rdSlab)
	var resp http1.Response
	hdrN, respBody, err := readResponseInto(uc, rdSlab.Bytes(), &resp)
	if err != nil {
		gwErr = true
		uc.MarkBroken()
		return w.WriteHeader(502, nil)
	}
	// Response header parsed — capture the status for the outlier
	// detector. The deferred closure at function entry feeds it to
	// pool.RecordResult on return.
	recordedStatus = int(resp.Status)
	// Bounded-zero: response read touched hdrN + len(respBody) bytes.
	rdSlab.MarkWritten(hdrN + len(respBody))

	// Build the H2 header fields from the H1 response.
	respIntent := irt.ExecuteResponse(routeSet, func(name string) string {
		return string(resp.Lookup([]byte(name)))
	})
	outFields := make([]http2.HeaderField, 0, resp.NumHeaders+respIntent.HeaderMutations.Len())
	src := resp.Src()
	for i := 0; i < resp.NumHeaders; i++ {
		name := resp.Headers[i].Name.Bytes(src)
		if isHopByHop(name) || intentHeaderRemoved(respIntent.HeaderMutations, name) ||
			intentHeaderOverridden(respIntent.HeaderMutations, name) {
			continue
		}
		outFields = append(outFields, http2.HeaderField{
			Name:  lowerString(name),
			Value: string(resp.Headers[i].Value.Bytes(src)),
		})
	}
	respIntent.HeaderMutations.Each(func(hm irt.HeaderMutation) bool {
		if hm.Remove || hm.Name == "" {
			return true
		}
		outFields = append(outFields, http2.HeaderField{Name: lowerString([]byte(hm.Name)), Value: hm.Value})
		return true
	})
	if alt := h.parent.AltSvc(); len(alt) > 0 {
		outFields = append(outFields, http2.HeaderField{Name: "alt-svc", Value: string(alt)})
	}
	if err := w.WriteHeader(int(resp.Status), outFields); err != nil {
		return err
	}

	// Body piggyback from the header read.
	if len(respBody) > 0 {
		if _, err := w.Write(respBody); err != nil {
			return err
		}
	}
	// Remaining body by framing. We use io.Copy with w as the sink —
	// the H2 respWriter chunks into max_frame_size DATA frames.
	switch {
	case resp.ContentLength > 0:
		remaining := resp.ContentLength - int64(len(respBody))
		if remaining > 0 {
			if _, err := io.CopyN(w, uc, remaining); err != nil {
				gwErr = true
				uc.MarkBroken()
				return err
			}
		}
	case resp.Chunked:
		if _, err := io.Copy(w, uc); err != nil {
			gwErr = true
			uc.MarkBroken()
			return err
		}
	case resp.Close:
		if _, err := io.Copy(w, uc); err != nil {
			gwErr = true
			uc.MarkBroken()
			return err
		}
		// Clean close-after-body: the upstream deliberately closed after
		// signalling Connection: close — not a failure. MarkBroken
		// forces the conn out of the pool but doesn't feed the detector.
		uc.MarkBroken()
	}
	return nil
}

// readResponseInto is a copy of the h1 package's readResponse, inlined
// here so we don't export it. Parses until the header block is
// complete, returning header-bytes consumed and piggy-back body slice.
func readResponseInto(uc net.Conn, rbuf []byte, resp *http1.Response) (int, []byte, error) {
	filled := 0
	for {
		n, err := uc.Read(rbuf[filled:])
		if n > 0 {
			filled += n
			consumed, perr := http1.ParseResponse(rbuf[:filled], resp)
			if perr == nil {
				return consumed, rbuf[consumed:filled], nil
			}
			if perr != http1.ErrNeedMore {
				return 0, nil, perr
			}
			if filled == len(rbuf) {
				return 0, nil, http1.ErrTooLarge
			}
		}
		if err != nil {
			return 0, nil, err
		}
	}
}

// lowerString downcases name and returns a string. Used because the H1
// parser keeps original casing; the H2 peer expects lowercase.
func lowerString(b []byte) string {
	buf := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return string(buf)
}

func toHeaderKVs(fields []http2.HeaderField) []headerKV {
	out := make([]headerKV, 0, len(fields))
	for _, f := range fields {
		out = append(out, headerKV{name: f.Name, value: f.Value})
	}
	return out
}
