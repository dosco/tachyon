package http1

import (
	"errors"
	"io"
)

// CopyChunkedBody reads a chunked Transfer-Encoding body from src,
// validating each chunk's framing, and writes the validated chunks to
// dst. Before reading from src, bodyHead is consumed from the front —
// typically the bytes that piggy-backed on the request-header read in
// the H1 handler.
//
// Contract:
//   - scratch is a caller-owned scratch buffer; we use it as a sliding
//     window. cap(scratch) must be at least 64 bytes (a slab is 4 KiB
//     or 16 KiB, both fine).
//   - The function returns nil on a well-formed body terminated by a
//     zero-size last-chunk + CRLF (RFC 7230 §4.1.1). Trailer headers
//     are NOT supported: their presence returns ErrMalformed. Clients
//     that send trailers are rare in practice; a future patch can
//     relax this.
//   - On malformed framing, returns ErrMalformed and dst may have been
//     partially written — callers should treat the upstream conn as
//     broken.
//   - On an I/O error from src or dst, that error is returned directly.
//
// The function writes bytes through verbatim after validating them, so
// a well-formed chunked body is re-emitted byte-for-byte. That means
// no rewriting cost beyond one scratch-buffer shift per chunk.
func CopyChunkedBody(dst io.Writer, src io.Reader, bodyHead, scratch []byte) error {
	if cap(scratch) < 64 {
		return errors.New("http1: scratch too small")
	}
	// s is our sliding window of validated-but-unwritten bytes. It is
	// always a prefix of scratch[:cap(scratch)] starting at index 0; we
	// shift remaining bytes to the front after each consume step.
	s := scratch[:0]
	s = append(s, bodyHead...)

	// fill reads more bytes from src into the tail of scratch, updating
	// s to reflect the new length. Returns io.ErrUnexpectedEOF if src
	// returned 0 bytes with a nil error (shouldn't happen) or EOF mid-stream.
	fill := func() error {
		if len(s) == cap(scratch) {
			// No room: a chunk size line or CRLF is larger than our
			// scratch. That would be an absurd chunk header; reject.
			return ErrTooLarge
		}
		n, err := src.Read(scratch[len(s):cap(scratch)])
		if n > 0 {
			s = scratch[:len(s)+n]
			return nil
		}
		if err == nil || errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	// consume(n) writes s[:n] to dst and shifts the tail down.
	consume := func(n int) error {
		if n == 0 {
			return nil
		}
		if _, err := dst.Write(s[:n]); err != nil {
			return err
		}
		copy(s, s[n:])
		s = s[:len(s)-n]
		return nil
	}

	for {
		// 1. Parse the chunk-size line. Read until ChunkHeader returns.
		var size int64
		var off int
		for {
			var herr error
			size, off, herr = ChunkHeader(s)
			if herr == nil {
				break
			}
			if !errors.Is(herr, ErrNeedMore) {
				return herr
			}
			if ferr := fill(); ferr != nil {
				return ferr
			}
		}
		// 2. Write the size line bytes verbatim.
		if err := consume(off); err != nil {
			return err
		}

		if size == 0 {
			// last-chunk. The RFC allows trailer headers here, but we
			// don't forward them; require the empty trailer (a lone
			// CRLF) and finish.
			for len(s) < 2 {
				if err := fill(); err != nil {
					return err
				}
			}
			if s[0] != '\r' || s[1] != '\n' {
				return ErrMalformed
			}
			return consume(2)
		}

		// 3. Stream `size` bytes of payload to dst.
		remaining := size
		for remaining > 0 {
			if len(s) > 0 {
				take := int64(len(s))
				if take > remaining {
					take = remaining
				}
				if err := consume(int(take)); err != nil {
					return err
				}
				remaining -= take
				continue
			}
			// Buffer empty. If we still need at least a full scratch,
			// read straight into scratch and pass through — avoids the
			// shift cost on big chunks.
			if remaining >= int64(cap(scratch)) {
				n, rerr := src.Read(scratch)
				if n > 0 {
					if _, werr := dst.Write(scratch[:n]); werr != nil {
						return werr
					}
					remaining -= int64(n)
					continue
				}
				if rerr == nil || errors.Is(rerr, io.EOF) {
					return io.ErrUnexpectedEOF
				}
				return rerr
			}
			if err := fill(); err != nil {
				return err
			}
		}

		// 4. Read and validate trailing CRLF after the payload.
		for len(s) < 2 {
			if err := fill(); err != nil {
				return err
			}
		}
		if s[0] != '\r' || s[1] != '\n' {
			return ErrMalformed
		}
		if err := consume(2); err != nil {
			return err
		}
	}
}
