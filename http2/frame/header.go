package frame

import "encoding/binary"

// Type is the HTTP/2 frame type. Values are the on-wire u8.
type Type uint8

const (
	TypeData         Type = 0x0
	TypeHeaders      Type = 0x1
	TypePriority     Type = 0x2
	TypeRSTStream    Type = 0x3
	TypeSettings     Type = 0x4
	TypePushPromise  Type = 0x5
	TypePing         Type = 0x6
	TypeGoAway       Type = 0x7
	TypeWindowUpdate Type = 0x8
	TypeContinuation Type = 0x9
)

// Flag is the HTTP/2 frame flags bitmap.
type Flag uint8

const (
	FlagDataEndStream      Flag = 0x01
	FlagDataPadded         Flag = 0x08
	FlagHeadersEndStream   Flag = 0x01
	FlagHeadersEndHeaders  Flag = 0x04
	FlagHeadersPadded      Flag = 0x08
	FlagHeadersPriority    Flag = 0x20
	FlagSettingsAck        Flag = 0x01
	FlagPingAck            Flag = 0x01
	FlagContinuationEndHdr Flag = 0x04
)

// HeaderSize is the fixed HTTP/2 frame header length in bytes.
const HeaderSize = 9

// MaxPayload is the maximum allowed payload per SETTINGS_MAX_FRAME_SIZE
// clamp we advertise (see settings.go). Kept here so framing code can
// sanity-check without importing the parent package.
const MaxPayload = 1 << 14 // 16 KiB — the RFC floor; we never negotiate higher.

// Header is a parsed 9-byte frame header.
//
//	+-----------------------------------------------+
//	|                 Length (24)                   |
//	+---------------+---------------+---------------+
//	|   Type (8)    |   Flags (8)   |
//	+-+-------------+---------------+-------------------------------+
//	|R|                 Stream Identifier (31)                      |
//	+=+=============================================================+
type Header struct {
	Length   uint32 // 24-bit; high byte always zero
	Type     Type
	Flags    Flag
	StreamID uint32 // 31-bit; the reserved top bit is masked off
}

// ReadHeader parses a 9-byte frame header from b. b must be exactly
// HeaderSize bytes; callers peek nine bytes off the wire, then read
// Length more for the payload.
func ReadHeader(b []byte) Header {
	// 24-bit length is big-endian across b[0..3].
	return Header{
		Length:   uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]),
		Type:     Type(b[3]),
		Flags:    Flag(b[4]),
		StreamID: binary.BigEndian.Uint32(b[5:9]) & 0x7fffffff,
	}
}

// AppendHeader writes h onto buf and returns the extended slice.
func AppendHeader(buf []byte, h Header) []byte {
	buf = append(buf,
		byte(h.Length>>16), byte(h.Length>>8), byte(h.Length),
		byte(h.Type), byte(h.Flags))
	var sid [4]byte
	binary.BigEndian.PutUint32(sid[:], h.StreamID&0x7fffffff)
	return append(buf, sid[:]...)
}

// Has reports whether f has bit mask set.
func (f Flag) Has(mask Flag) bool { return f&mask != 0 }
