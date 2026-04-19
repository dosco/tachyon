package http1

import "tachyon/buf"

// Parse parses one HTTP/1.1 request from src into r. On success it returns
// the number of bytes consumed (the offset of the body's first byte) and
// nil. If src is a truncated message, returns 0, ErrNeedMore; the caller
// keeps the buffer and calls again after reading more.
//
// No allocations. Headers and the request target are Spans into src; r.src
// is set so accessors can resolve them.
//
// The parser does not read the body. The caller, who knows whether it must
// forward or buffer the body, consumes from src[n:].
func Parse(src []byte, r *Request) (int, error) {
	r.Reset()
	r.src = src

	// --- Request-Line -----------------------------------------------------
	// method SP target SP HTTP/x.y CRLF

	lineEnd := findCRLF(src, 0)
	if lineEnd < 0 {
		return 0, ErrNeedMore
	}

	// Method.
	i := 0
	for i < lineEnd && src[i] != ' ' {
		if !isTokenChar(src[i]) {
			return 0, ErrMalformed
		}
		i++
	}
	if i == 0 || i >= lineEnd {
		return 0, ErrMalformed
	}
	r.Method = buf.Span{Off: 0, Len: uint32(i)}
	i++ // skip SP

	// Target. We accept any non-space printable bytes; origin-form and
	// absolute-form both satisfy that.
	pathStart := i
	for i < lineEnd && src[i] != ' ' {
		i++
	}
	if i == pathStart || i >= lineEnd {
		return 0, ErrMalformed
	}
	r.Path = buf.Span{Off: uint32(pathStart), Len: uint32(i - pathStart)}
	i++ // skip SP

	// HTTP-version. Only "HTTP/1.0" or "HTTP/1.1" are accepted.
	if lineEnd-i != 8 {
		return 0, ErrMalformed
	}
	ver := src[i:lineEnd]
	if ver[0] != 'H' || ver[1] != 'T' || ver[2] != 'T' || ver[3] != 'P' ||
		ver[4] != '/' || ver[5] != '1' || ver[6] != '.' {
		return 0, ErrMalformed
	}
	switch ver[7] {
	case '0':
		r.Minor = 0
		r.Close = true // HTTP/1.0 defaults to close unless Keep-Alive is negotiated
	case '1':
		r.Minor = 1
	default:
		return 0, ErrMalformed
	}

	// --- Headers ----------------------------------------------------------
	r.ContentLength = -1
	pos := lineEnd + 2 // past CRLF
	for {
		if pos+1 >= len(src) {
			return 0, ErrNeedMore
		}
		if src[pos] == '\r' && src[pos+1] == '\n' {
			// End of headers.
			pos += 2
			return pos, nil
		}
		n, err := parseHeader(src, pos, r)
		if err != nil {
			return 0, err
		}
		pos = n
	}
}

// parseHeader consumes one header line starting at pos. Returns the offset
// of the next line (just past CRLF) or an error.
func parseHeader(src []byte, pos int, r *Request) (int, error) {
	if r.NumHeaders >= MaxHeaders {
		return 0, ErrTooLarge
	}

	// field-name = token
	nameStart := pos
	for pos < len(src) && src[pos] != ':' {
		if !isTokenChar(src[pos]) {
			return 0, ErrMalformed
		}
		pos++
	}
	if pos == nameStart || pos >= len(src) {
		if pos >= len(src) {
			return 0, ErrNeedMore
		}
		return 0, ErrMalformed
	}
	nameEnd := pos
	pos++ // skip ':'

	// OWS before value.
	for pos < len(src) && (src[pos] == ' ' || src[pos] == '\t') {
		pos++
	}

	lineEnd := findCRLF(src, pos)
	if lineEnd < 0 {
		return 0, ErrNeedMore
	}

	// Trim trailing OWS.
	valEnd := lineEnd
	for valEnd > pos && (src[valEnd-1] == ' ' || src[valEnd-1] == '\t') {
		valEnd--
	}

	name := buf.Span{Off: uint32(nameStart), Len: uint32(nameEnd - nameStart)}
	val := buf.Span{Off: uint32(pos), Len: uint32(valEnd - pos)}

	// Record control headers that affect framing.
	nameBytes := name.Bytes(src)
	valBytes := val.Bytes(src)
	switch {
	case EqualFold(nameBytes, HdrContentLength):
		n := parseUint(valBytes)
		if n < 0 {
			return 0, ErrMalformed
		}
		r.ContentLength = n
	case EqualFold(nameBytes, HdrTransferEncode):
		// Only "chunked" is meaningful to us; anything else passes through
		// as a header and upstream handles it. If both CL and TE present,
		// TE wins per RFC 7230 §3.3.3.
		if EqualFold(valBytes, ValueChunked) {
			r.Chunked = true
			r.ContentLength = -1
		}
	case EqualFold(nameBytes, HdrConnection):
		if EqualFold(valBytes, ValueClose) {
			r.Close = true
		} else if r.Minor == 0 && EqualFold(valBytes, HdrKeepAlive) {
			r.Close = false
		}
	}

	r.Headers[r.NumHeaders] = Header{Name: name, Value: val}
	r.NumHeaders++
	return lineEnd + 2, nil
}
