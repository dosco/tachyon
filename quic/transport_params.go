package quic

import (
	"errors"

	"tachyon/quic/packet"
)

// PeerTransportParams holds the client-declared QUIC transport
// parameters we care about for flow control and recovery.
// Defaults per RFC 9000 §18.2 apply when the peer omits a parameter.
type PeerTransportParams struct {
	InitialMaxData                 uint64
	InitialMaxStreamDataBidiLocal  uint64
	InitialMaxStreamDataBidiRemote uint64
	InitialMaxStreamDataUni        uint64
	InitialMaxStreamsBidi          uint64
	InitialMaxStreamsUni           uint64
	MaxIdleTimeoutMS               uint64
	MaxUDPPayloadSize              uint64
	AckDelayExponent               uint64
	MaxAckDelayMS                  uint64
}

// defaultPeerTransportParams returns the RFC 9000 §18.2 defaults
// for parameters the peer may omit.
func defaultPeerTransportParams() PeerTransportParams {
	return PeerTransportParams{
		MaxUDPPayloadSize: 65527,
		AckDelayExponent:  3,
		MaxAckDelayMS:     25,
	}
}

// parsePeerTransportParams walks a TLS QUIC transport parameters
// extension and populates the flow-control / timing fields we care
// about. Unknown IDs are silently skipped (RFC 9000 §18.1).
func parsePeerTransportParams(b []byte) (PeerTransportParams, error) {
	tp := defaultPeerTransportParams()
	for len(b) > 0 {
		id, n, ok := packet.ReadVarint(b)
		if !ok {
			return tp, errors.New("quic: tp id truncated")
		}
		b = b[n:]
		length, n, ok := packet.ReadVarint(b)
		if !ok {
			return tp, errors.New("quic: tp len truncated")
		}
		b = b[n:]
		if uint64(len(b)) < length {
			return tp, errors.New("quic: tp truncated")
		}
		val := b[:length]
		b = b[length:]
		switch id {
		case 0x01:
			tp.MaxIdleTimeoutMS = mustVarint(val)
		case 0x03:
			tp.MaxUDPPayloadSize = mustVarint(val)
		case 0x04:
			tp.InitialMaxData = mustVarint(val)
		case 0x05:
			tp.InitialMaxStreamDataBidiLocal = mustVarint(val)
		case 0x06:
			tp.InitialMaxStreamDataBidiRemote = mustVarint(val)
		case 0x07:
			tp.InitialMaxStreamDataUni = mustVarint(val)
		case 0x08:
			tp.InitialMaxStreamsBidi = mustVarint(val)
		case 0x09:
			tp.InitialMaxStreamsUni = mustVarint(val)
		case 0x0a:
			tp.AckDelayExponent = mustVarint(val)
		case 0x0b:
			tp.MaxAckDelayMS = mustVarint(val)
		}
	}
	return tp, nil
}

func mustVarint(b []byte) uint64 {
	v, _, ok := packet.ReadVarint(b)
	if !ok {
		return 0
	}
	return v
}
