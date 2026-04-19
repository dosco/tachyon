package frame

import (
	"encoding/binary"
	"errors"
)

// Headers is the parsed shape of a HEADERS frame. Block is the raw HPACK
// fragment with optional padding and priority bytes already stripped; the
// HPACK decoder runs over it directly. Block aliases payload — copy if
// you need it to outlive the frame buffer.
type Headers struct {
	Block       []byte
	EndStream   bool
	EndHeaders  bool
	HasPriority bool
}

// ReadHeaders parses a HEADERS payload. The optional pad-length + priority
// structure makes this the fiddliest of the simple frames.
//
// Layout (all sections conditional on flags):
//
//	[Pad Length (8) if PADDED]
//	[Stream Dependency (31) if PRIORITY]
//	[Weight (8)              if PRIORITY]
//	 Header Block Fragment (*)
//	 Padding (*)            if PADDED
func ReadHeaders(flags Flag, payload []byte) (Headers, error) {
	p := payload
	var (
		padLen int
		h      Headers
	)
	h.EndStream = flags.Has(FlagHeadersEndStream)
	h.EndHeaders = flags.Has(FlagHeadersEndHeaders)
	h.HasPriority = flags.Has(FlagHeadersPriority)

	if flags.Has(FlagHeadersPadded) {
		if len(p) == 0 {
			return Headers{}, errors.New("http2: padded HEADERS missing pad-length byte")
		}
		padLen = int(p[0])
		p = p[1:]
	}
	if h.HasPriority {
		if len(p) < 5 {
			return Headers{}, errors.New("http2: HEADERS priority block truncated")
		}
		p = p[5:] // stream dep (4) + weight (1); discarded per policy
	}
	if padLen > len(p) {
		return Headers{}, errors.New("http2: HEADERS pad-length > remaining payload")
	}
	h.Block = p[:len(p)-padLen]
	return h, nil
}

// AppendHeaders emits a HEADERS frame with the given block. No padding,
// no priority. The 24-bit length field can't hold more than 16 MiB; the
// caller fragments across CONTINUATION if they need more.
func AppendHeaders(buf []byte, streamID uint32, endStream, endHeaders bool, block []byte) []byte {
	var flags Flag
	if endStream {
		flags |= FlagHeadersEndStream
	}
	if endHeaders {
		flags |= FlagHeadersEndHeaders
	}
	buf = AppendHeader(buf, Header{
		Length: uint32(len(block)), Type: TypeHeaders, Flags: flags, StreamID: streamID,
	})
	return append(buf, block...)
}

// AppendContinuation emits a CONTINUATION frame carrying the given block
// fragment. Used after a HEADERS (or another CONTINUATION) that didn't
// carry END_HEADERS.
func AppendContinuation(buf []byte, streamID uint32, endHeaders bool, block []byte) []byte {
	var flags Flag
	if endHeaders {
		flags = FlagContinuationEndHdr
	}
	buf = AppendHeader(buf, Header{
		Length: uint32(len(block)), Type: TypeContinuation, Flags: flags, StreamID: streamID,
	})
	return append(buf, block...)
}

// unused but kept to document the encoded layout of a priority block.
var _ = binary.BigEndian
