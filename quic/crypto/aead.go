package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

// PacketProtector bundles the AEAD and header-protection machinery for
// one direction of one encryption level. Callers hold two of these per
// active keying level (client tx / client rx — or server tx / server rx
// from the peer's point of view).
type PacketProtector struct {
	aead     cipher.AEAD
	hpCipher cipher.Block
	iv       []byte // 12 bytes
}

// NewAESGCMProtector builds a protector for the TLS_AES_128_GCM_SHA256
// cipher suite — the only suite required for Initial and the default
// negotiated suite for 1-RTT in nearly all modern TLS stacks.
func NewAESGCMProtector(s Secrets) (*PacketProtector, error) {
	if len(s.Key) != 16 {
		return nil, fmt.Errorf("quic/crypto: aes-128-gcm expects 16-byte key, got %d", len(s.Key))
	}
	if len(s.IV) != 12 {
		return nil, fmt.Errorf("quic/crypto: aead iv must be 12 bytes, got %d", len(s.IV))
	}
	if len(s.HP) != 16 {
		return nil, fmt.Errorf("quic/crypto: aes-128 hp key must be 16 bytes, got %d", len(s.HP))
	}
	blk, err := aes.NewCipher(s.Key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	hpBlk, err := aes.NewCipher(s.HP)
	if err != nil {
		return nil, err
	}
	return &PacketProtector{aead: aead, hpCipher: hpBlk, iv: s.IV}, nil
}

// Overhead returns the size of the AEAD authentication tag (16 bytes
// for both AES-GCM and ChaCha20-Poly1305).
func (p *PacketProtector) Overhead() int { return p.aead.Overhead() }

// nonce derives the per-packet AEAD nonce by XOR'ing the static IV with
// the 62-bit packet number placed in the low bytes (RFC 9001 §5.3).
func (p *PacketProtector) nonce(out []byte, pn uint64) []byte {
	copy(out, p.iv)
	for i := 0; i < 8; i++ {
		out[len(p.iv)-1-i] ^= byte(pn >> (8 * i))
	}
	return out
}

// Seal encrypts payload in place-ish: it returns the ciphertext (which
// includes the AEAD tag) for the given packet number and header. The
// associated data is the on-wire header before header protection is
// applied. Callers assemble the final packet by concatenating header ||
// sealed.
func (p *PacketProtector) Seal(dst, header, payload []byte, pn uint64) []byte {
	var nonce [12]byte
	p.nonce(nonce[:], pn)
	return p.aead.Seal(dst, nonce[:], payload, header)
}

// Open decrypts ciphertext (which must include the AEAD tag) and writes
// the plaintext to dst, returning the resulting slice. Header is the
// associated data (the on-wire header with header protection already
// removed).
func (p *PacketProtector) Open(dst, header, ciphertext []byte, pn uint64) ([]byte, error) {
	var nonce [12]byte
	p.nonce(nonce[:], pn)
	return p.aead.Open(dst, nonce[:], ciphertext, header)
}

// HeaderProtectionMask returns a 5-byte mask for the given AEAD sample
// (the first 16 bytes of the ciphertext starting 4 bytes after the
// start of the packet number field). The mask is applied to bits of
// the first byte and to the packet-number bytes per RFC 9001 §5.4.
func (p *PacketProtector) HeaderProtectionMask(sample []byte) ([]byte, error) {
	if len(sample) < p.hpCipher.BlockSize() {
		return nil, fmt.Errorf("quic/crypto: hp sample must be %d bytes, got %d",
			p.hpCipher.BlockSize(), len(sample))
	}
	var out [16]byte
	p.hpCipher.Encrypt(out[:], sample[:p.hpCipher.BlockSize()])
	return out[:5], nil
}
