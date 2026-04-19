package frame

import (
	"encoding/binary"
	"errors"
)

// ErrInvalidWindowUpdate is returned when a WINDOW_UPDATE payload carries
// a zero increment (RFC 7540 §6.9 — must be > 0).
var ErrInvalidWindowUpdate = errors.New("http2: WINDOW_UPDATE zero increment")

// ReadWindowUpdate parses a WINDOW_UPDATE payload. Returns the 31-bit
// increment.
func ReadWindowUpdate(payload []byte) (uint32, error) {
	if len(payload) != 4 {
		return 0, errors.New("http2: WINDOW_UPDATE payload length != 4")
	}
	inc := binary.BigEndian.Uint32(payload) & 0x7fffffff
	if inc == 0 {
		return 0, ErrInvalidWindowUpdate
	}
	return inc, nil
}

// AppendWindowUpdate emits a WINDOW_UPDATE. streamID=0 applies to the
// connection; otherwise to a specific stream.
func AppendWindowUpdate(buf []byte, streamID, increment uint32) []byte {
	buf = AppendHeader(buf, Header{
		Length: 4, Type: TypeWindowUpdate, StreamID: streamID,
	})
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], increment&0x7fffffff)
	return append(buf, b[:]...)
}
