package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Protector is the minimum surface quic/packet needs from quic/crypto to
// apply packet + header protection. Defined here as an interface so
// quic/packet stays free of the crypto package (avoids an import cycle
// with the TLS driver).
type Protector interface {
	Overhead() int
	Seal(dst, header, payload []byte, pn uint64) []byte
	Open(dst, header, ciphertext []byte, pn uint64) ([]byte, error)
	HeaderProtectionMask(sample []byte) ([]byte, error)
}

// InitialPacket is the plaintext input to SealInitial. All fields are
// required. Token may be nil for server-sent Initials.
type InitialPacket struct {
	Version      uint32
	DCID         []byte
	SCID         []byte
	Token        []byte
	PacketNumber uint64
	PacketNumLen int    // 1..4 — length of the on-wire pn field
	Payload      []byte // one or more encoded frames
}

// SealInitial builds a fully protected Initial packet: header | AEAD
// seal(payload) with header protection applied.
func SealInitial(dst []byte, p Protector, in InitialPacket) ([]byte, error) {
	if in.PacketNumLen < 1 || in.PacketNumLen > 4 {
		return nil, fmt.Errorf("quic/packet: invalid packet number length %d", in.PacketNumLen)
	}
	if p == nil {
		return nil, errors.New("quic/packet: nil protector")
	}
	protector := p

	// First byte: long form (0x80) + fixed bit (0x40) + type=Initial (0<<4)
	// + reserved (00) + pn_len-1 in low 2 bits.
	first := byte(0xc0) | byte(in.PacketNumLen-1)

	header := make([]byte, 0, 64+len(in.Token)+len(in.DCID)+len(in.SCID))
	header = append(header, first)
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], in.Version)
	header = append(header, v[:]...)
	header = append(header, byte(len(in.DCID)))
	header = append(header, in.DCID...)
	header = append(header, byte(len(in.SCID)))
	header = append(header, in.SCID...)
	header = AppendVarint(header, uint64(len(in.Token)))
	header = append(header, in.Token...)

	// Length covers packet number + encrypted payload + AEAD tag.
	payloadLen := in.PacketNumLen + len(in.Payload) + protector.Overhead()
	header = AppendVarint(header, uint64(payloadLen))

	pnOffset := len(header)
	// Append the unprotected packet number bytes.
	for i := in.PacketNumLen - 1; i >= 0; i-- {
		header = append(header, byte(in.PacketNumber>>(8*uint(i))))
	}

	// Seal the payload. Associated data = header with unprotected pn.
	sealed := protector.Seal(nil, header, in.Payload, in.PacketNumber)

	// Assemble full packet.
	pkt := append([]byte(nil), header...)
	pkt = append(pkt, sealed...)

	// Apply header protection. Sample is the 16 bytes starting 4 bytes
	// past the start of the packet number field (RFC 9001 §5.4.2).
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(pkt) {
		return nil, fmt.Errorf("quic/packet: ciphertext too short to sample")
	}
	mask, err := protector.HeaderProtectionMask(pkt[sampleStart : sampleStart+16])
	if err != nil {
		return nil, err
	}
	applyHeaderProtection(pkt, pnOffset, in.PacketNumLen, mask, true)

	return append(dst, pkt...), nil
}

// OpenInitial reverses SealInitial: removes header protection, decodes
// the packet number, AEAD-decrypts the payload, and returns the frames
// plus the parsed header (with the recovered packet number in a new
// field). The expectedPNSpace is the next-expected packet number in the
// Initial space and is used to reconstruct the full 62-bit pn from the
// on-wire truncation.
func OpenInitial(buf []byte, p Protector, expectedPNSpace uint64) (Header, []byte, uint64, error) {
	if p == nil {
		return Header{}, nil, 0, errors.New("quic/packet: nil protector")
	}
	protector := p

	h, err := Parse(buf, 0)
	if err != nil {
		return Header{}, nil, 0, err
	}
	if h.Form != FormLong || h.Type != LongInitial {
		return Header{}, nil, 0, errors.New("quic/packet: not an Initial")
	}

	pnOffset := h.PayloadOffset
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return Header{}, nil, 0, ErrShort
	}
	mask, err := protector.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return Header{}, nil, 0, err
	}

	// Copy the packet so we can mutate the header in place.
	pkt := append([]byte(nil), buf...)
	// Strip HP on first byte to recover pn_len.
	pkt[0] ^= mask[0] & 0x0f
	pnLen := int(pkt[0]&0x03) + 1
	// Strip HP on packet-number bytes.
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}

	// Recover truncated pn then restore full 62-bit value against
	// expectedPNSpace.
	truncatedPN := uint64(0)
	for i := 0; i < pnLen; i++ {
		truncatedPN = (truncatedPN << 8) | uint64(pkt[pnOffset+i])
	}
	pn := decodePacketNumber(expectedPNSpace, truncatedPN, pnLen)

	// Associated data = header with unprotected pn.
	aadEnd := pnOffset + pnLen
	header := pkt[:aadEnd]
	// Ciphertext = remainder of the Length region.
	ciphertextLen := int(h.Length) - pnLen
	if ciphertextLen < protector.Overhead() {
		return Header{}, nil, 0, ErrShort
	}
	if aadEnd+ciphertextLen > len(pkt) {
		return Header{}, nil, 0, ErrShort
	}
	plaintext, err := protector.Open(nil, header, pkt[aadEnd:aadEnd+ciphertextLen], pn)
	if err != nil {
		return Header{}, nil, 0, err
	}
	return h, plaintext, pn, nil
}

// applyHeaderProtection XORs the first-byte low bits and the packet-
// number bytes with the 5-byte mask. When sealing=true the input first
// byte is the clear pn_len value; sealing=false is the same operation.
func applyHeaderProtection(pkt []byte, pnOffset, pnLen int, mask []byte, _ bool) {
	pkt[0] ^= mask[0] & 0x0f // long header: protect low 4 bits
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
}

// decodePacketNumber reconstructs a 62-bit packet number from its
// truncated on-wire form given the largest expected value (RFC 9000
// §A.3).
func decodePacketNumber(largestExpected, truncated uint64, pnLen int) uint64 {
	pnNBits := uint(pnLen * 8)
	pnWin := uint64(1) << pnNBits
	pnHWin := pnWin / 2
	pnMask := pnWin - 1
	expectedPN := largestExpected + 1
	candidate := (expectedPN &^ pnMask) | truncated
	// Use signed comparisons (via explicit overflow checks) so small
	// expectedPN values don't wrap when subtracting pnHWin.
	if candidate+pnHWin <= expectedPN && candidate < (1<<62)-pnWin {
		candidate += pnWin
	} else if candidate > expectedPN+pnHWin && candidate >= pnWin {
		candidate -= pnWin
	}
	return candidate
}
