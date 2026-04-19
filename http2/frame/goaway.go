package frame

import (
	"encoding/binary"
	"errors"
)

// GoAway is the parsed shape of a GOAWAY frame.
type GoAway struct {
	LastStreamID uint32
	Code         ErrCode
	Debug        []byte // aliases into the source buffer; zero-copy
}

// ReadGoAway parses a GOAWAY payload. The debug data span aliases into
// payload — caller must copy if it needs to outlive the frame buffer.
func ReadGoAway(payload []byte) (GoAway, error) {
	if len(payload) < 8 {
		return GoAway{}, errors.New("http2: GOAWAY payload length < 8")
	}
	return GoAway{
		LastStreamID: binary.BigEndian.Uint32(payload[:4]) & 0x7fffffff,
		Code:         ErrCode(binary.BigEndian.Uint32(payload[4:8])),
		Debug:        payload[8:],
	}, nil
}

// AppendGoAway emits a GOAWAY with the given fields. debug may be nil.
func AppendGoAway(buf []byte, lastStreamID uint32, code ErrCode, debug []byte) []byte {
	buf = AppendHeader(buf, Header{
		Length: uint32(8 + len(debug)), Type: TypeGoAway, StreamID: 0,
	})
	var b [8]byte
	binary.BigEndian.PutUint32(b[0:4], lastStreamID&0x7fffffff)
	binary.BigEndian.PutUint32(b[4:8], uint32(code))
	buf = append(buf, b[:]...)
	return append(buf, debug...)
}
