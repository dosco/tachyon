package quic

// Test-only helpers exported for use by cross-package tests (e.g.
// http3/h3_test.go). They wrap the unexported sealHandshake /
// sealShort and mirror the long/short-header opening logic from the
// in-package echo test. Kept in a non-test file so external _test
// packages can link against them; not part of the public QUIC API.

import (
	"tachyon/quic/crypto"
	"tachyon/quic/packet"
)

// TestClientTransportParams returns a minimal valid set of client
// transport parameters for use in loopback tests. Includes
// initial_source_connection_id + the flow-control limits the server
// needs to send response data on client-initiated bidi streams.
func TestClientTransportParams(scid []byte) []byte {
	out := append([]byte(nil),
		0x0f, byte(len(scid)))
	out = append(out, scid...)
	appendVarintParam := func(dst []byte, id, v uint64) []byte {
		dst = packet.AppendVarint(dst, id)
		val := packet.AppendVarint(nil, v)
		dst = packet.AppendVarint(dst, uint64(len(val)))
		return append(dst, val...)
	}
	out = appendVarintParam(out, 0x04, 1<<20)  // initial_max_data
	out = appendVarintParam(out, 0x05, 1<<16)  // initial_max_stream_data_bidi_local
	out = appendVarintParam(out, 0x06, 1<<16)  // initial_max_stream_data_bidi_remote
	out = appendVarintParam(out, 0x07, 1<<16)  // initial_max_stream_data_uni
	out = appendVarintParam(out, 0x08, 100)    // initial_max_streams_bidi
	out = appendVarintParam(out, 0x09, 100)    // initial_max_streams_uni
	out = appendVarintParam(out, 0x01, 30_000) // max_idle_timeout
	return out
}

// TestSealHandshake builds a 1-RTT-precursor Handshake-level long packet.
func TestSealHandshake(p *crypto.PacketProtector, dcid, scid []byte, pn uint64, pnLen int, payload []byte) ([]byte, error) {
	return sealHandshake(p, handshakePacket{
		Version:      packet.Version1,
		DCID:         dcid,
		SCID:         scid,
		PacketNumber: pn,
		PacketNumLen: pnLen,
		Payload:      payload,
	})
}

// TestSealShort builds a 1-RTT short-header packet.
func TestSealShort(p *crypto.PacketProtector, dcid []byte, pn uint64, pnLen int, payload []byte) ([]byte, error) {
	return sealShort(p, dcid, pn, pnLen, payload)
}

// TestOpenLong removes header protection, decrypts a long-header packet,
// and returns the plaintext and recovered packet number.
func TestOpenLong(buf []byte, h packet.Header, p *crypto.PacketProtector, expected uint64) ([]byte, uint64, error) {
	pnOffset := h.PayloadOffset
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return nil, 0, packet.ErrShort
	}
	mask, err := p.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return nil, 0, err
	}
	pkt := append([]byte(nil), buf...)
	pkt[0] ^= mask[0] & 0x0f
	pnLen := int(pkt[0]&0x03) + 1
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	truncated := uint64(0)
	for i := 0; i < pnLen; i++ {
		truncated = (truncated << 8) | uint64(pkt[pnOffset+i])
	}
	pn := decodePNCompat(expected, truncated, pnLen)
	aadEnd := pnOffset + pnLen
	ctLen := int(h.Length) - pnLen
	plain, err := p.Open(nil, pkt[:aadEnd], pkt[aadEnd:aadEnd+ctLen], pn)
	return plain, pn, err
}

// TestOpenShort removes header protection and decrypts a short-header
// packet. scid is the DCID the client chose for the server (so it
// knows the DCID length in the incoming short header).
func TestOpenShort(p *crypto.PacketProtector, scid, buf []byte, expected uint64) ([]byte, uint64, error) {
	dcidLen := len(scid)
	pnOffset := 1 + dcidLen
	sampleStart := pnOffset + 4
	if sampleStart+16 > len(buf) {
		return nil, 0, packet.ErrShort
	}
	mask, err := p.HeaderProtectionMask(buf[sampleStart : sampleStart+16])
	if err != nil {
		return nil, 0, err
	}
	pkt := append([]byte(nil), buf...)
	pkt[0] ^= mask[0] & 0x1f
	pnLen := int(pkt[0]&0x03) + 1
	for i := 0; i < pnLen; i++ {
		pkt[pnOffset+i] ^= mask[1+i]
	}
	truncated := uint64(0)
	for i := 0; i < pnLen; i++ {
		truncated = (truncated << 8) | uint64(pkt[pnOffset+i])
	}
	pn := decodePNCompat(expected, truncated, pnLen)
	aadEnd := pnOffset + pnLen
	plain, err := p.Open(nil, pkt[:aadEnd], pkt[aadEnd:], pn)
	return plain, pn, err
}
