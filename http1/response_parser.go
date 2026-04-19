package http1

import "tachyon/buf"

// ParseResponse parses a single HTTP/1.1 response from src into r and returns
// the byte offset of the first body byte on success. Semantics mirror Parse.
func ParseResponse(src []byte, r *Response) (int, error) {
	r.Reset()
	r.src = src

	// Status-Line: HTTP/1.x SP code SP reason CRLF
	lineEnd := findCRLF(src, 0)
	if lineEnd < 0 {
		return 0, ErrNeedMore
	}

	if lineEnd < 12 {
		return 0, ErrMalformed
	}
	if src[0] != 'H' || src[1] != 'T' || src[2] != 'T' || src[3] != 'P' ||
		src[4] != '/' || src[5] != '1' || src[6] != '.' || src[8] != ' ' {
		return 0, ErrMalformed
	}
	switch src[7] {
	case '0':
		r.Minor = 0
		r.Close = true
	case '1':
		r.Minor = 1
	default:
		return 0, ErrMalformed
	}

	// Status code: 3 ASCII digits.
	if src[9] < '0' || src[9] > '9' ||
		src[10] < '0' || src[10] > '9' ||
		src[11] < '0' || src[11] > '9' {
		return 0, ErrMalformed
	}
	r.Status = uint16(src[9]-'0')*100 + uint16(src[10]-'0')*10 + uint16(src[11]-'0')

	reasonStart := 13 // skip ' '
	if reasonStart > lineEnd {
		reasonStart = lineEnd
	}
	r.Reason = buf.Span{Off: uint32(reasonStart), Len: uint32(lineEnd - reasonStart)}

	// Headers - same shape as request.
	r.ContentLength = -1
	pos := lineEnd + 2
	for {
		if pos+1 >= len(src) {
			return 0, ErrNeedMore
		}
		if src[pos] == '\r' && src[pos+1] == '\n' {
			return pos + 2, nil
		}
		n, err := parseResponseHeader(src, pos, r)
		if err != nil {
			return 0, err
		}
		pos = n
	}
}

func parseResponseHeader(src []byte, pos int, r *Response) (int, error) {
	if r.NumHeaders >= MaxHeaders {
		return 0, ErrTooLarge
	}

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
	pos++

	for pos < len(src) && (src[pos] == ' ' || src[pos] == '\t') {
		pos++
	}

	lineEnd := findCRLF(src, pos)
	if lineEnd < 0 {
		return 0, ErrNeedMore
	}
	valEnd := lineEnd
	for valEnd > pos && (src[valEnd-1] == ' ' || src[valEnd-1] == '\t') {
		valEnd--
	}

	name := buf.Span{Off: uint32(nameStart), Len: uint32(nameEnd - nameStart)}
	val := buf.Span{Off: uint32(pos), Len: uint32(valEnd - pos)}

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
		if EqualFold(valBytes, ValueChunked) {
			r.Chunked = true
			r.ContentLength = -1
		}
	case EqualFold(nameBytes, HdrConnection):
		if EqualFold(valBytes, ValueClose) {
			r.Close = true
		}
	}

	r.Headers[r.NumHeaders] = Header{Name: name, Value: val}
	r.NumHeaders++
	return lineEnd + 2, nil
}
