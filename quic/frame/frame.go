// Package frame parses and encodes the QUIC frames tachyon needs to
// complete the Initial + Handshake exchange.
//
// Scope for Phase 2:
//   - PADDING  (0x00)
//   - PING     (0x01)
//   - ACK      (0x02, 0x03 — with ECN counts)
//   - CRYPTO   (0x06)
//   - CONNECTION_CLOSE (0x1c = protocol-level, 0x1d = application-level)
//
// Stream-level frames (STREAM, RESET_STREAM, MAX_*, etc.) belong to
// Phase 3 and are not implemented here.
package frame

import (
	"errors"
	"fmt"

	"tachyon/quic/packet"
)

// Type is a 62-bit varint frame type code (RFC 9000 §12).
type Type uint64

const (
	TypePadding         Type = 0x00
	TypePing            Type = 0x01
	TypeAck             Type = 0x02
	TypeAckECN          Type = 0x03
	TypeCrypto          Type = 0x06
	TypeConnClose       Type = 0x1c
	TypeConnCloseAppErr Type = 0x1d
)

// Common errors.
var (
	ErrTruncated   = errors.New("quic/frame: truncated frame")
	ErrUnknownType = errors.New("quic/frame: unknown frame type for encryption level")
)

// Crypto is a CRYPTO frame (RFC 9000 §19.6). Offset is the byte offset
// within the crypto stream; Data is the handshake-message bytes.
type Crypto struct {
	Offset uint64
	Data   []byte
}

// Ack is an ACK / ACK-ECN frame (RFC 9000 §19.3).
type Ack struct {
	LargestAcked uint64
	AckDelay     uint64 // microseconds, scaled by 2^ack_delay_exponent
	AckRanges    []AckRange
	ECN          *ECNCounts // nil for ACK (0x02); populated for ACK_ECN (0x03)
}

// AckRange is one contiguous run of acknowledged packet numbers.
// Smallest .. Largest inclusive.
type AckRange struct {
	Smallest uint64
	Largest  uint64
}

// ECNCounts is the optional trailer on an ACK_ECN frame.
type ECNCounts struct {
	ECT0, ECT1, CE uint64
}

// ConnectionClose is a CONNECTION_CLOSE frame (RFC 9000 §19.19). When
// IsApplication is true, the frame type is 0x1d and FrameType is not
// present on the wire.
type ConnectionClose struct {
	ErrorCode     uint64
	IsApplication bool
	FrameType     uint64 // 0 for 0x1d frames
	Reason        []byte
}

// Visitor consumes each parsed frame; implement only the callbacks you
// need. A non-nil error aborts parsing.
type Visitor struct {
	OnPadding         func()
	OnPing            func()
	OnAck             func(Ack) error
	OnCrypto          func(Crypto) error
	OnConnectionClose func(ConnectionClose) error
	OnStream          func(Stream) error
	OnResetStream     func(ResetStream) error
	OnStopSending     func(StopSending) error
	OnMaxData         func(MaxData) error
	OnMaxStreamData   func(MaxStreamData) error
	OnMaxStreams      func(MaxStreams) error
	OnHandshakeDone   func() error
}

// Parse walks a decrypted packet payload, dispatching to the Visitor.
// Unknown frame types return ErrUnknownType.
func Parse(payload []byte, v Visitor) error {
	for len(payload) > 0 {
		raw, n, ok := packet.ReadVarint(payload)
		if !ok {
			return ErrTruncated
		}
		typ := Type(raw)
		payload = payload[n:]

		switch typ {
		case TypePadding:
			// Any run of 0x00 bytes; each counts as its own frame but we
			// just squash them to one visitor call for caller sanity.
			if v.OnPadding != nil {
				v.OnPadding()
			}
		case TypePing:
			if v.OnPing != nil {
				v.OnPing()
			}
		case TypeAck, TypeAckECN:
			ack, rest, err := parseAck(payload, typ == TypeAckECN)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnAck != nil {
				if err := v.OnAck(ack); err != nil {
					return err
				}
			}
		case TypeCrypto:
			cr, rest, err := parseCrypto(payload)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnCrypto != nil {
				if err := v.OnCrypto(cr); err != nil {
					return err
				}
			}
		case TypeConnClose, TypeConnCloseAppErr:
			cc, rest, err := parseConnClose(payload, typ == TypeConnCloseAppErr)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnConnectionClose != nil {
				if err := v.OnConnectionClose(cc); err != nil {
					return err
				}
			}
		case TypeResetStream:
			rs, rest, err := parseResetStream(payload)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnResetStream != nil {
				if err := v.OnResetStream(rs); err != nil {
					return err
				}
			}
		case TypeStopSending:
			ss, rest, err := parseStopSending(payload)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnStopSending != nil {
				if err := v.OnStopSending(ss); err != nil {
					return err
				}
			}
		case TypeMaxData:
			m, rest, err := parseSimpleVarint(payload)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnMaxData != nil {
				if err := v.OnMaxData(MaxData{Max: m}); err != nil {
					return err
				}
			}
		case TypeMaxStreamData:
			m, rest, err := parseMaxStreamData(payload)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnMaxStreamData != nil {
				if err := v.OnMaxStreamData(m); err != nil {
					return err
				}
			}
		case TypeMaxStreamsBidi, TypeMaxStreamsUni:
			m, rest, err := parseSimpleVarint(payload)
			if err != nil {
				return err
			}
			payload = rest
			if v.OnMaxStreams != nil {
				if err := v.OnMaxStreams(MaxStreams{Bidi: typ == TypeMaxStreamsBidi, Max: m}); err != nil {
					return err
				}
			}
		case TypeHandshakeDone:
			if v.OnHandshakeDone != nil {
				if err := v.OnHandshakeDone(); err != nil {
					return err
				}
			}
		default:
			if typ >= TypeStreamBase && typ <= TypeStreamMax {
				s, rest, err := parseStream(payload, byte(typ))
				if err != nil {
					return err
				}
				payload = rest
				if v.OnStream != nil {
					if err := v.OnStream(s); err != nil {
						return err
					}
				}
				continue
			}
			return fmt.Errorf("%w: type=0x%x", ErrUnknownType, uint64(typ))
		}
	}
	return nil
}

func parseCrypto(b []byte) (Crypto, []byte, error) {
	offset, n, ok := packet.ReadVarint(b)
	if !ok {
		return Crypto{}, nil, ErrTruncated
	}
	b = b[n:]
	length, n, ok := packet.ReadVarint(b)
	if !ok {
		return Crypto{}, nil, ErrTruncated
	}
	b = b[n:]
	if uint64(len(b)) < length {
		return Crypto{}, nil, ErrTruncated
	}
	return Crypto{Offset: offset, Data: b[:length]}, b[length:], nil
}

func parseAck(b []byte, withECN bool) (Ack, []byte, error) {
	largest, n, ok := packet.ReadVarint(b)
	if !ok {
		return Ack{}, nil, ErrTruncated
	}
	b = b[n:]
	delay, n, ok := packet.ReadVarint(b)
	if !ok {
		return Ack{}, nil, ErrTruncated
	}
	b = b[n:]
	rangeCount, n, ok := packet.ReadVarint(b)
	if !ok {
		return Ack{}, nil, ErrTruncated
	}
	b = b[n:]
	firstRange, n, ok := packet.ReadVarint(b)
	if !ok {
		return Ack{}, nil, ErrTruncated
	}
	b = b[n:]

	if firstRange > largest {
		return Ack{}, nil, fmt.Errorf("quic/frame: ack first-range underflow")
	}
	smallest := largest - firstRange
	a := Ack{LargestAcked: largest, AckDelay: delay}
	a.AckRanges = append(a.AckRanges, AckRange{Smallest: smallest, Largest: largest})

	for i := uint64(0); i < rangeCount; i++ {
		gap, n, ok := packet.ReadVarint(b)
		if !ok {
			return Ack{}, nil, ErrTruncated
		}
		b = b[n:]
		length, n, ok := packet.ReadVarint(b)
		if !ok {
			return Ack{}, nil, ErrTruncated
		}
		b = b[n:]
		// Next range is just below the previous smallest with `gap+1`
		// unacked packets in between (RFC 9000 §19.3.1).
		if smallest < gap+2+length {
			return Ack{}, nil, fmt.Errorf("quic/frame: ack gap/length underflow")
		}
		largestHere := smallest - gap - 2
		smallestHere := largestHere - length
		a.AckRanges = append(a.AckRanges, AckRange{Smallest: smallestHere, Largest: largestHere})
		smallest = smallestHere
	}

	if withECN {
		ecn := ECNCounts{}
		ecn.ECT0, n, ok = packet.ReadVarint(b)
		if !ok {
			return Ack{}, nil, ErrTruncated
		}
		b = b[n:]
		ecn.ECT1, n, ok = packet.ReadVarint(b)
		if !ok {
			return Ack{}, nil, ErrTruncated
		}
		b = b[n:]
		ecn.CE, n, ok = packet.ReadVarint(b)
		if !ok {
			return Ack{}, nil, ErrTruncated
		}
		b = b[n:]
		a.ECN = &ecn
	}
	return a, b, nil
}

func parseConnClose(b []byte, isApp bool) (ConnectionClose, []byte, error) {
	errCode, n, ok := packet.ReadVarint(b)
	if !ok {
		return ConnectionClose{}, nil, ErrTruncated
	}
	b = b[n:]
	cc := ConnectionClose{ErrorCode: errCode, IsApplication: isApp}
	if !isApp {
		ft, n, ok := packet.ReadVarint(b)
		if !ok {
			return ConnectionClose{}, nil, ErrTruncated
		}
		cc.FrameType = ft
		b = b[n:]
	}
	reasonLen, n, ok := packet.ReadVarint(b)
	if !ok {
		return ConnectionClose{}, nil, ErrTruncated
	}
	b = b[n:]
	if uint64(len(b)) < reasonLen {
		return ConnectionClose{}, nil, ErrTruncated
	}
	cc.Reason = b[:reasonLen]
	return cc, b[reasonLen:], nil
}

// ---------- encoders ----------

// AppendCrypto appends an encoded CRYPTO frame to dst.
func AppendCrypto(dst []byte, c Crypto) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeCrypto))
	dst = packet.AppendVarint(dst, c.Offset)
	dst = packet.AppendVarint(dst, uint64(len(c.Data)))
	dst = append(dst, c.Data...)
	return dst
}

// AppendAck appends an ACK frame covering a single contiguous range
// [smallest..largest]. This is the common case during handshake where
// we have no out-of-order packets yet.
func AppendAck(dst []byte, largest, smallest, ackDelay uint64) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeAck))
	dst = packet.AppendVarint(dst, largest)
	dst = packet.AppendVarint(dst, ackDelay)
	dst = packet.AppendVarint(dst, 0) // range count (0 extra ranges)
	dst = packet.AppendVarint(dst, largest-smallest)
	return dst
}

// AppendConnectionClose appends a protocol-level CONNECTION_CLOSE.
func AppendConnectionClose(dst []byte, errCode, frameType uint64, reason string) []byte {
	dst = packet.AppendVarint(dst, uint64(TypeConnClose))
	dst = packet.AppendVarint(dst, errCode)
	dst = packet.AppendVarint(dst, frameType)
	dst = packet.AppendVarint(dst, uint64(len(reason)))
	dst = append(dst, reason...)
	return dst
}

// AppendPadding appends n PADDING bytes.
func AppendPadding(dst []byte, n int) []byte {
	for i := 0; i < n; i++ {
		dst = append(dst, 0x00)
	}
	return dst
}
