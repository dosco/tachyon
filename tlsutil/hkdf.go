// HKDF-Expand-Label for TLS 1.3 traffic-key derivation (RFC 8446 §7.1).
//
// Given an application traffic secret captured from a KeyLogWriter, we
// derive the AEAD key and IV that kTLS's setsockopt needs:
//
//   key = HKDF-Expand-Label(secret, "key", "", key_len)
//   iv  = HKDF-Expand-Label(secret, "iv",  "", 12)
//
// The "length" and "label" construction follows RFC 8446:
//
//   struct {
//       uint16 length;                       // big-endian
//       opaque label<7..255> = "tls13 " + Label;
//       opaque context<0..255> = Context;
//   } HkdfLabel;
//
// For kTLS we always pass context = "" (the empty byte string). The
// label prefix is literally "tls13 " (6 bytes, ASCII space after 13).
//
// We only need HKDF-Expand (single-block output for key/iv sizes ≤ 48),
// so the full HKDF-Extract-then-Expand isn't needed here — the traffic
// secret is already the Expand input.

package tlsutil

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"hash"
)

// HashID identifies the PRF hash for a cipher suite. kTLS only cares
// about the two we'd see on the wire: SHA-256 (AES-128-GCM, CHACHA20)
// and SHA-384 (AES-256-GCM).
type HashID uint8

const (
	HashSHA256 HashID = iota
	HashSHA384
)

func (h HashID) new() hash.Hash {
	switch h {
	case HashSHA384:
		return sha512.New384()
	default:
		return sha256.New()
	}
}

// HKDFExpandLabel derives `length` bytes using the TLS 1.3 labeled
// HKDF-Expand form defined in RFC 8446 §7.1. context is almost always
// empty for kTLS key/iv derivation.
func HKDFExpandLabel(h HashID, secret []byte, label string, context []byte, length int) []byte {
	// Build HkdfLabel on the stack-ish — small, bounded sizes.
	fullLabel := "tls13 " + label
	info := make([]byte, 0, 4+len(fullLabel)+len(context))
	info = append(info, byte(length>>8), byte(length))
	info = append(info, byte(len(fullLabel)))
	info = append(info, fullLabel...)
	info = append(info, byte(len(context)))
	info = append(info, context...)
	return hkdfExpand(h, secret, info, length)
}

// hkdfExpand is the standard RFC 5869 HKDF-Expand. PRK = secret, info
// = info, L = length. T(0) = "", T(i) = HMAC(PRK, T(i-1) | info | i).
func hkdfExpand(h HashID, prk, info []byte, length int) []byte {
	mac := hmac.New(func() hash.Hash { return h.new() }, prk)
	hashLen := mac.Size()
	n := (length + hashLen - 1) / hashLen
	out := make([]byte, 0, n*hashLen)
	var prev []byte
	for i := 1; i <= n; i++ {
		mac.Reset()
		mac.Write(prev)
		mac.Write(info)
		mac.Write([]byte{byte(i)})
		prev = mac.Sum(prev[:0])
		out = append(out, prev...)
	}
	return out[:length]
}
