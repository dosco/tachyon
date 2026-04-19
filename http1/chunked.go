package http1

// Chunked transfer coding helpers. Minimal surface: we only need enough to
// stream a body forward without buffering it whole.
//
// The reader returns the extent of one chunk payload inside the input buffer.
// The writer emits the chunk-size prefix and terminator around a payload.

// ChunkHeader parses the chunk-size line at the start of p. Returns:
//   - size: the payload size in bytes (0 for the last chunk)
//   - off:  the offset of the payload's first byte within p
//   - err:  ErrNeedMore if the size line isn't complete, ErrMalformed otherwise
//
// Chunk extensions (";name=value") are accepted and discarded. Trailers after
// the zero-size chunk are left to the caller.
func ChunkHeader(p []byte) (size int64, off int, err error) {
	i := 0
	for i < len(p) {
		c := p[i]
		switch {
		case c >= '0' && c <= '9':
			size = size<<4 | int64(c-'0')
		case c >= 'a' && c <= 'f':
			size = size<<4 | int64(c-'a'+10)
		case c >= 'A' && c <= 'F':
			size = size<<4 | int64(c-'A'+10)
		case c == ';' || c == '\r' || c == ' ' || c == '\t':
			goto done
		default:
			return 0, 0, ErrMalformed
		}
		if size < 0 {
			return 0, 0, ErrMalformed
		}
		i++
	}
	return 0, 0, ErrNeedMore

done:
	// Scan to CRLF.
	end := findCRLF(p, i)
	if end < 0 {
		return 0, 0, ErrNeedMore
	}
	return size, end + 2, nil
}

// AppendChunk writes a chunk framing around payload to dst. Useful for
// chunk-encoding an arbitrary body we produced ourselves (error responses).
func AppendChunk(dst, payload []byte) []byte {
	dst = AppendChunkSize(dst, len(payload))
	dst = append(dst, payload...)
	dst = append(dst, CRLF...)
	return dst
}

// AppendChunkSize writes a chunk-size line ("N\r\n" in hex) to dst.
// Useful when the caller wants to stream the payload separately
// (e.g. to avoid copying a large body into a framing buffer) — write
// the size line, then the payload, then CRLF.
func AppendChunkSize(dst []byte, n int) []byte {
	const hex = "0123456789abcdef"
	var sizeBuf [16]byte
	si := len(sizeBuf)
	if n == 0 {
		si--
		sizeBuf[si] = '0'
	} else {
		for v := n; v > 0; v >>= 4 {
			si--
			sizeBuf[si] = hex[v&0xf]
		}
	}
	dst = append(dst, sizeBuf[si:]...)
	dst = append(dst, CRLF...)
	return dst
}

// LastChunk writes the zero-size chunk that terminates a chunked body.
func LastChunk(dst []byte) []byte {
	return append(dst, '0', '\r', '\n', '\r', '\n')
}
