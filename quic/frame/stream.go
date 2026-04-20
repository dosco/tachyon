package frame

import (
	"fmt"

	"tachyon/quic/packet"
)

// Stream frame type range (RFC 9000 §19.8):
//
//	0b00001XXX — STREAM frames. Low 3 bits are the OFF/LEN/FIN flags.
//	    bit 0 (FIN): end of stream.
//	    bit 1 (LEN): explicit length field present.
//	    bit 2 (OFF): explicit offset field present.
const (
	TypeStreamBase Type = 0x08
	TypeStreamMax  Type = 0x0f

	TypeResetStream  Type = 0x04
	TypeStopSending  Type = 0x05
	TypeMaxData      Type = 0x10
	TypeMaxStreamData Type = 0x11
	TypeMaxStreamsBidi Type = 0x12
	TypeMaxStreamsUni  Type = 0x13
	TypeDataBlocked       Type = 0x14
	TypeStreamDataBlocked Type = 0x15
	TypeStreamsBlockedBidi Type = 0x16
	TypeStreamsBlockedUni  Type = 0x17
	TypeNewConnectionID   Type = 0x18
	TypeRetireConnectionID Type = 0x19
	TypePathChallenge     Type = 0x1a
	TypePathResponse      Type = 0x1b
	TypeHandshakeDone     Type = 0x1e
)

// Stream is a decoded STREAM frame.
type Stream struct {
	StreamID uint64
	Offset   uint64
	Data     []byte
	Fin      bool
}

// ResetStream is a RESET_STREAM frame (RFC 9000 §19.4).
type ResetStream struct {
	StreamID    uint64
	ErrorCode   uint64
	FinalSize   uint64
}

// StopSending is a STOP_SENDING frame (RFC 9000 §19.5).
type StopSending struct {
	StreamID  uint64
	ErrorCode uint64
}

// MaxData is a MAX_DATA frame (RFC 9000 §19.9).
type MaxData struct{ Max uint64 }

// MaxStreamData is a MAX_STREAM_DATA frame (RFC 9000 §19.10).
type MaxStreamData struct {
	StreamID uint64
	Max      uint64
}

// MaxStreams is a MAX_STREAMS frame (RFC 9000 §19.11).
type MaxStreams struct {
	Bidi bool
	Max  uint64
}

// -- parsers -----------------------------------------------------------

func parseStream(b []byte, typeCode byte) (Stream, []byte, error) {
	hasOff := typeCode&0x04 != 0
	hasLen := typeCode&0x02 != 0
	fin := typeCode&0x01 != 0

	sid, n, ok := packet.ReadVarint(b)
	if !ok {
		return Stream{}, nil, ErrTruncated
	}
	b = b[n:]
	var offset uint64
	if hasOff {
		offset, n, ok = packet.ReadVarint(b)
		if !ok {
			return Stream{}, nil, ErrTruncated
		}
		b = b[n:]
	}
	var data []byte
	if hasLen {
		length, n, ok := packet.ReadVarint(b)
		if !ok {
			return Stream{}, nil, ErrTruncated
		}
		b = b[n:]
		if uint64(len(b)) < length {
			return Stream{}, nil, ErrTruncated
		}
		data = b[:length]
		b = b[length:]
	} else {
		// Implicit-length: STREAM extends to end of packet payload.
		data = b
		b = nil
	}
	return Stream{StreamID: sid, Offset: offset, Data: data, Fin: fin}, b, nil
}

func parseResetStream(b []byte) (ResetStream, []byte, error) {
	sid, n, ok := packet.ReadVarint(b)
	if !ok {
		return ResetStream{}, nil, ErrTruncated
	}
	b = b[n:]
	code, n, ok := packet.ReadVarint(b)
	if !ok {
		return ResetStream{}, nil, ErrTruncated
	}
	b = b[n:]
	fsz, n, ok := packet.ReadVarint(b)
	if !ok {
		return ResetStream{}, nil, ErrTruncated
	}
	b = b[n:]
	return ResetStream{StreamID: sid, ErrorCode: code, FinalSize: fsz}, b, nil
}

func parseStopSending(b []byte) (StopSending, []byte, error) {
	sid, n, ok := packet.ReadVarint(b)
	if !ok {
		return StopSending{}, nil, ErrTruncated
	}
	b = b[n:]
	code, n, ok := packet.ReadVarint(b)
	if !ok {
		return StopSending{}, nil, ErrTruncated
	}
	b = b[n:]
	return StopSending{StreamID: sid, ErrorCode: code}, b, nil
}

func parseSimpleVarint(b []byte) (uint64, []byte, error) {
	v, n, ok := packet.ReadVarint(b)
	if !ok {
		return 0, nil, ErrTruncated
	}
	return v, b[n:], nil
}

func parseMaxStreamData(b []byte) (MaxStreamData, []byte, error) {
	sid, n, ok := packet.ReadVarint(b)
	if !ok {
		return MaxStreamData{}, nil, ErrTruncated
	}
	b = b[n:]
	m, n, ok := packet.ReadVarint(b)
	if !ok {
		return MaxStreamData{}, nil, ErrTruncated
	}
	return MaxStreamData{StreamID: sid, Max: m}, b[n:], nil
}

// -- encoders ----------------------------------------------------------

// AppendStream appends a STREAM frame. If len(data) > 0 the frame always
// includes an explicit length so it can be coalesced with following
// frames. Offset is always included (callers operating near zero-offset
// can still pay the cost — the saving is one byte).
func AppendStream(dst []byte, s Stream) []byte {
	typ := byte(TypeStreamBase) | 0x04 | 0x02 // OFF + LEN
	if s.Fin {
		typ |= 0x01
	}
	dst = append(dst, typ)
	dst = packet.AppendVarint(dst, s.StreamID)
	dst = packet.AppendVarint(dst, s.Offset)
	dst = packet.AppendVarint(dst, uint64(len(s.Data)))
	dst = append(dst, s.Data...)
	return dst
}

// AppendResetStream appends a RESET_STREAM frame.
func AppendResetStream(dst []byte, r ResetStream) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeResetStream))
	dst = packet.AppendVarint(dst, r.StreamID)
	dst = packet.AppendVarint(dst, r.ErrorCode)
	dst = packet.AppendVarint(dst, r.FinalSize)
	return dst
}

// AppendStopSending appends a STOP_SENDING frame.
func AppendStopSending(dst []byte, s StopSending) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeStopSending))
	dst = packet.AppendVarint(dst, s.StreamID)
	dst = packet.AppendVarint(dst, s.ErrorCode)
	return dst
}

// AppendMaxData appends a MAX_DATA frame.
func AppendMaxData(dst []byte, m uint64) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeMaxData))
	dst = packet.AppendVarint(dst, m)
	return dst
}

// AppendMaxStreamData appends a MAX_STREAM_DATA frame.
func AppendMaxStreamData(dst []byte, m MaxStreamData) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeMaxStreamData))
	dst = packet.AppendVarint(dst, m.StreamID)
	dst = packet.AppendVarint(dst, m.Max)
	return dst
}

// AppendMaxStreams appends a MAX_STREAMS frame for bidi or uni streams.
func AppendMaxStreams(dst []byte, m MaxStreams) []byte {
	t := TypeMaxStreamsUni
	if m.Bidi {
		t = TypeMaxStreamsBidi
	}
	dst = packet.AppendVarint(dst, uint64(t))
	dst = packet.AppendVarint(dst, m.Max)
	return dst
}

// AppendHandshakeDone appends a HANDSHAKE_DONE frame (server→client only).
func AppendHandshakeDone(dst []byte) []byte {
	return packet.AppendVarint(dst, uint64(TypeHandshakeDone))
}

// unknownStreamTypeErr is returned only when we detect a known frame
// type that the Visitor did not ask about and we cannot safely skip it
// (because the frame has no self-describing length).
func unknownStreamTypeErr(t Type) error {
	return fmt.Errorf("%w: type=0x%x", ErrUnknownType, uint64(t))
}
