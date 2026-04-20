// Package frame implements the HTTP/3 frame layer (RFC 9114 §7).
//
// Frame format on the wire:
//
//	Type  (varint)
//	Length (varint) — byte count of Payload
//	Payload (opaque)
//
// Scope for Phase 4: DATA (0x00), HEADERS (0x01), SETTINGS (0x04),
// GOAWAY (0x07). CANCEL_PUSH, PUSH_PROMISE, MAX_PUSH_ID, and reserved
// "grease" types are out of scope — the proxy doesn't do server push.
package frame

import (
	"errors"
	"fmt"

	"tachyon/quic/packet"
)

// Type is a 62-bit varint frame type code.
type Type uint64

const (
	TypeData     Type = 0x00
	TypeHeaders  Type = 0x01
	TypeSettings Type = 0x04
	TypeGoAway   Type = 0x07
)

var (
	ErrTruncated   = errors.New("http3/frame: truncated")
	ErrUnknownType = errors.New("http3/frame: unknown type on stream")
)

// SettingID is a 62-bit varint identifying a SETTINGS parameter.
type SettingID uint64

const (
	SettingQPACKMaxTableCapacity SettingID = 0x01
	SettingMaxFieldSectionSize   SettingID = 0x06
	SettingQPACKBlockedStreams   SettingID = 0x07
)

// Settings is a decoded SETTINGS frame.
type Settings map[SettingID]uint64

// GoAway payload is a single varint: the largest stream ID the peer
// will process (client→server) or the largest push ID (server→client).
type GoAway struct{ StreamOrPushID uint64 }

// Frame is a generic typed frame envelope, returned by Parse. For
// DATA/HEADERS the Payload points into the caller's buffer.
type Frame struct {
	Type    Type
	Payload []byte
}

// Parse decodes a single frame from b. Returns the frame, the number
// of bytes consumed, and ok=false plus ErrTruncated on short input.
func Parse(b []byte) (Frame, int, error) {
	typ, n, ok := packet.ReadVarint(b)
	if !ok {
		return Frame{}, 0, ErrTruncated
	}
	off := n
	length, n, ok := packet.ReadVarint(b[off:])
	if !ok {
		return Frame{}, 0, ErrTruncated
	}
	off += n
	if uint64(len(b)-off) < length {
		return Frame{}, 0, ErrTruncated
	}
	f := Frame{Type: Type(typ), Payload: b[off : off+int(length)]}
	return f, off + int(length), nil
}

// ParseSettings decodes the body of a SETTINGS frame.
func ParseSettings(body []byte) (Settings, error) {
	out := Settings{}
	for len(body) > 0 {
		id, n, ok := packet.ReadVarint(body)
		if !ok {
			return nil, ErrTruncated
		}
		body = body[n:]
		val, n, ok := packet.ReadVarint(body)
		if !ok {
			return nil, ErrTruncated
		}
		body = body[n:]
		out[SettingID(id)] = val
	}
	return out, nil
}

// AppendFrame appends Type|Length|payload to dst.
func AppendFrame(dst []byte, t Type, payload []byte) []byte {
	dst = packet.AppendVarint(dst, uint64(t))
	dst = packet.AppendVarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

// AppendSettings appends a SETTINGS frame carrying the given params.
// Deterministic ordering (map iteration order is randomized, so we
// use the natural varint-sorted order for golden tests upstream).
func AppendSettings(dst []byte, s Settings) []byte {
	var body []byte
	// Emit in a stable order for reproducibility — ascending by ID.
	ids := make([]uint64, 0, len(s))
	for id := range s {
		ids = append(ids, uint64(id))
	}
	sortVarints(ids)
	for _, id := range ids {
		body = packet.AppendVarint(body, id)
		body = packet.AppendVarint(body, s[SettingID(id)])
	}
	return AppendFrame(dst, TypeSettings, body)
}

// AppendGoAway appends a GOAWAY frame.
func AppendGoAway(dst []byte, g GoAway) []byte {
	body := packet.AppendVarint(nil, g.StreamOrPushID)
	return AppendFrame(dst, TypeGoAway, body)
}

func sortVarints(xs []uint64) {
	// Small sets (a handful of settings) — insertion sort keeps alloc
	// count at zero and the loop is trivial to audit.
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// Friendly error helper for dispatch code that wants to report an
// unexpected frame-type on a particular stream.
func Unexpected(t Type, onStream string) error {
	return fmt.Errorf("http3/frame: %w: type=0x%x on %s stream", ErrUnknownType, uint64(t), onStream)
}
