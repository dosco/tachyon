// Package crypto implements the QUIC-specific pieces of TLS 1.3 key
// schedule and packet protection from RFC 9001.
//
// What lives here:
//   - HKDF-Expand-Label (RFC 8446 §7.1), specialised for the short labels
//     that QUIC uses ("quic key", "quic iv", "quic hp", "client in",
//     "server in").
//   - Initial secret derivation (RFC 9001 §5.2) from the client's
//     destination connection ID.
//   - AEAD construction for the four QUIC cipher suites.
//   - Header protection mask generation.
//
// What does NOT live here: frame parsing, congestion control, TLS
// handshake driving. Callers stitch those together using the Secrets
// and AEAD types below.
package crypto

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
)

// HKDFExpandLabel implements the TLS 1.3 HKDF-Expand-Label construction
// (RFC 8446 §7.1). The label is prefixed with "tls13 " on the wire; QUIC
// uses the same prefix per RFC 9001 §5.1.
func HKDFExpandLabel(hashFn func() hash.Hash, secret []byte, label string, context []byte, length int) ([]byte, error) {
	full := "tls13 " + label
	if len(full) > 255 {
		return nil, fmt.Errorf("quic/crypto: label too long: %q", label)
	}
	if len(context) > 255 {
		return nil, fmt.Errorf("quic/crypto: context too long (%d bytes)", len(context))
	}
	// HkdfLabel = uint16 length || opaque label<7..255> || opaque context<0..255>
	info := make([]byte, 0, 2+1+len(full)+1+len(context))
	var ll [2]byte
	binary.BigEndian.PutUint16(ll[:], uint16(length))
	info = append(info, ll[:]...)
	info = append(info, byte(len(full)))
	info = append(info, full...)
	info = append(info, byte(len(context)))
	info = append(info, context...)
	return hkdf.Expand(hashFn, secret, string(info), length)
}

// InitialSalt is the version-1 initial salt from RFC 9001 §5.2.
var InitialSalt = []byte{
	0x38, 0x76, 0x2c, 0xf7, 0xf5, 0x59, 0x34, 0xb3,
	0x4d, 0x17, 0x9a, 0xe6, 0xa4, 0xc8, 0x0c, 0xad,
	0xcc, 0xbb, 0x7f, 0x0a,
}

// InitialSecret derives the connection's initial secret from the
// client's destination connection ID (RFC 9001 §5.2).
func InitialSecret(clientDCID []byte) []byte {
	s, err := hkdf.Extract(sha256.New, clientDCID, InitialSalt)
	if err != nil {
		// sha256 + ≤20-byte DCID can never exceed Extract's limits.
		panic(fmt.Sprintf("quic/crypto: unexpected hkdf extract failure: %v", err))
	}
	return s
}

// Secrets holds a set of packet-protection keys derived from a single
// handshake secret (initial, handshake, or 1-RTT). Initial packets use
// AES-128-GCM with SHA-256; callers override KeyLen/IVLen/HPLen when
// other cipher suites are negotiated.
type Secrets struct {
	Secret []byte // full traffic secret (kept for key update)
	Key    []byte
	IV     []byte // 12 bytes
	HP     []byte // header protection key
}

// DeriveInitialSecrets produces the server- and client-side initial
// secrets for a given client destination connection ID. Keys are
// AES-128-GCM (16-byte key, 12-byte IV, 16-byte HP key).
func DeriveInitialSecrets(clientDCID []byte) (client, server Secrets, err error) {
	is := InitialSecret(clientDCID)
	c, err := hkdfTrafficSecrets(is, "client in")
	if err != nil {
		return Secrets{}, Secrets{}, err
	}
	s, err := hkdfTrafficSecrets(is, "server in")
	if err != nil {
		return Secrets{}, Secrets{}, err
	}
	return c, s, nil
}

// SecretsFromTLS builds a set of packet-protection keys from a traffic
// secret emitted by crypto/tls (QUICSetReadSecret / QUICSetWriteSecret)
// for the given cipher suite. Only TLS_AES_128_GCM_SHA256 (0x1301) is
// wired for Phase 2.
func SecretsFromTLS(suite uint16, secret []byte) Secrets {
	switch suite {
	case 0x1301: // TLS_AES_128_GCM_SHA256
		key, err := HKDFExpandLabel(sha256.New, secret, "quic key", nil, 16)
		if err != nil {
			panic(err)
		}
		iv, err := HKDFExpandLabel(sha256.New, secret, "quic iv", nil, 12)
		if err != nil {
			panic(err)
		}
		hp, err := HKDFExpandLabel(sha256.New, secret, "quic hp", nil, 16)
		if err != nil {
			panic(err)
		}
		return Secrets{Secret: append([]byte(nil), secret...), Key: key, IV: iv, HP: hp}
	default:
		panic(fmt.Sprintf("quic/crypto: unsupported cipher suite 0x%04x", suite))
	}
}

func hkdfTrafficSecrets(initialSecret []byte, label string) (Secrets, error) {
	secret, err := HKDFExpandLabel(sha256.New, initialSecret, label, nil, sha256.Size)
	if err != nil {
		return Secrets{}, err
	}
	key, err := HKDFExpandLabel(sha256.New, secret, "quic key", nil, 16)
	if err != nil {
		return Secrets{}, err
	}
	iv, err := HKDFExpandLabel(sha256.New, secret, "quic iv", nil, 12)
	if err != nil {
		return Secrets{}, err
	}
	hp, err := HKDFExpandLabel(sha256.New, secret, "quic hp", nil, 16)
	if err != nil {
		return Secrets{}, err
	}
	return Secrets{Secret: secret, Key: key, IV: iv, HP: hp}, nil
}
