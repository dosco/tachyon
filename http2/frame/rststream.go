package frame

import (
	"encoding/binary"
	"errors"
)

// ErrCode is the HTTP/2 error code surfaced by RST_STREAM and GOAWAY
// (RFC 7540 §7).
type ErrCode uint32

const (
	ErrCodeNoError            ErrCode = 0x0
	ErrCodeProtocolError      ErrCode = 0x1
	ErrCodeInternalError      ErrCode = 0x2
	ErrCodeFlowControlError   ErrCode = 0x3
	ErrCodeSettingsTimeout    ErrCode = 0x4
	ErrCodeStreamClosed       ErrCode = 0x5
	ErrCodeFrameSizeError     ErrCode = 0x6
	ErrCodeRefusedStream      ErrCode = 0x7
	ErrCodeCancel             ErrCode = 0x8
	ErrCodeCompressionError   ErrCode = 0x9
	ErrCodeConnectError       ErrCode = 0xa
	ErrCodeEnhanceYourCalm    ErrCode = 0xb
	ErrCodeInadequateSecurity ErrCode = 0xc
	ErrCodeHTTP11Required     ErrCode = 0xd
)

// ReadRSTStream parses a 4-byte RST_STREAM payload.
func ReadRSTStream(payload []byte) (ErrCode, error) {
	if len(payload) != 4 {
		return 0, errors.New("http2: RST_STREAM payload length != 4")
	}
	return ErrCode(binary.BigEndian.Uint32(payload)), nil
}

// AppendRSTStream emits a RST_STREAM for streamID with the given code.
func AppendRSTStream(buf []byte, streamID uint32, code ErrCode) []byte {
	buf = AppendHeader(buf, Header{
		Length: 4, Type: TypeRSTStream, StreamID: streamID,
	})
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(code))
	return append(buf, b[:]...)
}
