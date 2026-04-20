// Package packet parses the unprotected portion of QUIC packet headers.
//
// Only header-form detection, version read, and connection-ID extraction
// are implemented. Packet-number recovery and payload decryption require
// the keys negotiated during the handshake and live in quic/crypto.
//
// References:
//   - RFC 9000 §17.2 (long header), §17.3 (short header), §17.2.1 (Version
//     Negotiation), §17.2.5 (Retry).
package packet

import (
	"encoding/binary"
	"errors"
)

// Version1 is the only version tachyon negotiates at this time.
const Version1 uint32 = 0x00000001

// Form is the QUIC header form.
type Form uint8

const (
	FormShort Form = 0
	FormLong  Form = 1
)

// LongType is the 2-bit packet-type field in a v1 long header.
type LongType uint8

const (
	LongInitial   LongType = 0x0
	LongZeroRTT   LongType = 0x1
	LongHandshake LongType = 0x2
	LongRetry     LongType = 0x3
)

// MaxConnIDLen is the per-RFC 9000 limit on connection-ID length for
// version 1.
const MaxConnIDLen = 20

// Header is a parsed, still-protected QUIC packet header. The packet
// number is not recovered here — it is protected by header protection
// and must be removed after key derivation.
type Header struct {
	Form    Form
	Version uint32 // 0 for short-header packets
	Type    LongType
	DCID    []byte
	SCID    []byte // empty for short-header packets
	// Token is the Retry / Initial token bytes, when present.
	Token []byte
	// PayloadOffset is the offset into the original packet where the
	// length-prefixed payload (long header) or protected packet-number +
	// payload (short header) begins.
	PayloadOffset int
	// Length is the variable-length integer "Length" field from a long
	// header (covers the packet number + payload). Zero for short
	// headers and for Version Negotiation / Retry packets.
	Length uint64
}

// IsLongHeader reports whether the first byte indicates a long header.
// Works even before full parsing; cheap enough for the packet demux hot path.
func IsLongHeader(b byte) bool { return b&0x80 != 0 }

// Errors returned by Parse.
var (
	ErrShort          = errors.New("quic/packet: buffer shorter than minimum header")
	ErrFixedBit       = errors.New("quic/packet: fixed bit not set")
	ErrConnIDTooLong  = errors.New("quic/packet: connection ID exceeds 20 bytes")
	ErrBadVarint      = errors.New("quic/packet: truncated variable-length integer")
	ErrVersionNeg     = errors.New("quic/packet: version negotiation packet")
	ErrUnknownVersion = errors.New("quic/packet: unsupported version")
)

// Parse reads as much of the header as is possible without the keys. It
// returns the header and the on-wire length consumed up to PayloadOffset.
//
// For short-header packets, the destination connection ID length is not
// self-describing on the wire; callers must pass dcidLen (the local
// connection-ID length tachyon chose when accepting the connection).
func Parse(buf []byte, dcidLen int) (Header, error) {
	if len(buf) < 1 {
		return Header{}, ErrShort
	}
	first := buf[0]
	if !IsLongHeader(first) {
		return parseShort(buf, dcidLen)
	}
	return parseLong(buf)
}

func parseLong(buf []byte) (Header, error) {
	// Long header layout (v1):
	//   1 byte  first byte  (form=1, fixed=1, type=2, reserved=2, pn_len=2 — last 4 bits protected)
	//   4 bytes version
	//   1 byte  dcid len
	//   N bytes dcid
	//   1 byte  scid len
	//   N bytes scid
	//   type-specific fields
	if len(buf) < 7 {
		return Header{}, ErrShort
	}
	first := buf[0]
	version := binary.BigEndian.Uint32(buf[1:5])

	// Version Negotiation packets carry version 0. They are sent by
	// clients, not received by servers, but we surface a sentinel so the
	// endpoint can drop them without treating the unknown version path.
	if version == 0 {
		h := Header{Form: FormLong, Version: 0}
		off := 5
		dcidLen := int(buf[off])
		off++
		if dcidLen > MaxConnIDLen || len(buf) < off+dcidLen+1 {
			return Header{}, ErrShort
		}
		h.DCID = buf[off : off+dcidLen]
		off += dcidLen
		scidLen := int(buf[off])
		off++
		if scidLen > MaxConnIDLen || len(buf) < off+scidLen {
			return Header{}, ErrShort
		}
		h.SCID = buf[off : off+scidLen]
		off += scidLen
		h.PayloadOffset = off
		return h, ErrVersionNeg
	}

	// Fixed bit (bit 0x40) MUST be 1 for v1 packets that aren't Version
	// Negotiation. Grease-QUIC-Bit (RFC 9287) relaxes this but we reject
	// for now.
	if first&0x40 == 0 {
		return Header{}, ErrFixedBit
	}
	ltype := LongType((first & 0x30) >> 4)

	off := 5
	if len(buf) < off+1 {
		return Header{}, ErrShort
	}
	dcidLen := int(buf[off])
	off++
	if dcidLen > MaxConnIDLen {
		return Header{}, ErrConnIDTooLong
	}
	if len(buf) < off+dcidLen+1 {
		return Header{}, ErrShort
	}
	dcid := buf[off : off+dcidLen]
	off += dcidLen
	scidLen := int(buf[off])
	off++
	if scidLen > MaxConnIDLen {
		return Header{}, ErrConnIDTooLong
	}
	if len(buf) < off+scidLen {
		return Header{}, ErrShort
	}
	scid := buf[off : off+scidLen]
	off += scidLen

	h := Header{
		Form:    FormLong,
		Version: version,
		Type:    ltype,
		DCID:    dcid,
		SCID:    scid,
	}

	switch ltype {
	case LongInitial:
		// Token Length (varint) + Token + Length (varint).
		tokLen, n, ok := ReadVarint(buf[off:])
		if !ok {
			return Header{}, ErrBadVarint
		}
		off += n
		if uint64(len(buf)-off) < tokLen {
			return Header{}, ErrShort
		}
		h.Token = buf[off : off+int(tokLen)]
		off += int(tokLen)
		length, n, ok := ReadVarint(buf[off:])
		if !ok {
			return Header{}, ErrBadVarint
		}
		off += n
		h.Length = length
		h.PayloadOffset = off
	case LongZeroRTT, LongHandshake:
		// Length (varint).
		length, n, ok := ReadVarint(buf[off:])
		if !ok {
			return Header{}, ErrBadVarint
		}
		off += n
		h.Length = length
		h.PayloadOffset = off
	case LongRetry:
		// No Length field. Rest of packet is Retry Token + 16-byte
		// Retry Integrity Tag. Callers handle the tag separately.
		h.PayloadOffset = off
	}
	return h, nil
}

func parseShort(buf []byte, dcidLen int) (Header, error) {
	// Short header: first byte 01SRRKPP (S=spin, R=reserved, K=key phase,
	// P=pn length). All trailing 5 bits are protected.
	if len(buf) < 1+dcidLen {
		return Header{}, ErrShort
	}
	if buf[0]&0x40 == 0 {
		return Header{}, ErrFixedBit
	}
	h := Header{
		Form:          FormShort,
		DCID:          buf[1 : 1+dcidLen],
		PayloadOffset: 1 + dcidLen,
	}
	return h, nil
}

// ReadVarint decodes an RFC 9000 §16 variable-length integer. Returns the
// value, the number of bytes consumed, and ok=false on truncation.
func ReadVarint(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	prefix := b[0] >> 6
	size := 1 << prefix // 1, 2, 4, or 8
	if len(b) < size {
		return 0, 0, false
	}
	v := uint64(b[0] & 0x3f)
	for i := 1; i < size; i++ {
		v = (v << 8) | uint64(b[i])
	}
	return v, size, true
}

// AppendVarint appends an RFC 9000 §16 varint-encoded value to dst.
func AppendVarint(dst []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(dst, byte(v))
	case v < 1<<14:
		return append(dst, byte(0x40|(v>>8)), byte(v))
	case v < 1<<30:
		return append(dst,
			byte(0x80|(v>>24)), byte(v>>16), byte(v>>8), byte(v))
	case v < 1<<62:
		return append(dst,
			byte(0xc0|(v>>56)), byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	default:
		panic("quic/packet: varint value out of range")
	}
}
