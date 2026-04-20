package packet

import (
	"crypto/rand"
	"encoding/binary"
)

// BuildVersionNegotiation assembles a Version Negotiation packet
// (RFC 9000 §17.2.1). clientDCID becomes the response's SCID and
// clientSCID becomes the response's DCID — i.e. connection IDs are
// swapped so the client can match the reply to its connection attempt.
//
// The supported list is the versions we are willing to speak; the first
// byte has the high bit set (long form) and bit 0x40 (fixed bit)
// deliberately off — endpoints MUST ignore the fixed bit on VN packets
// (§17.2.1). The 6 low bits of the first byte are unused and filled
// with a random value to exercise version-invariant parsers on the
// client side.
func BuildVersionNegotiation(dst, clientDCID, clientSCID []byte, supported []uint32) []byte {
	var r [1]byte
	_, _ = rand.Read(r[:])
	dst = append(dst, 0x80|(r[0]&0x7f))
	dst = append(dst, 0, 0, 0, 0) // Version = 0 signals VN.
	// Swap DCID/SCID from the client's packet.
	dst = append(dst, byte(len(clientSCID)))
	dst = append(dst, clientSCID...)
	dst = append(dst, byte(len(clientDCID)))
	dst = append(dst, clientDCID...)
	var v [4]byte
	for _, ver := range supported {
		binary.BigEndian.PutUint32(v[:], ver)
		dst = append(dst, v[:]...)
	}
	return dst
}
