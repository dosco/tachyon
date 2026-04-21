// Fallback stub for non-Linux builds. Keeps the public API surface
// stable so callers don't need their own build tag. On Linux, kTLS
// ships by default (see ktls_linux.go) — this file is not compiled.

//go:build !linux

package tlsutil

import "errors"

// ErrKTLSUnavailable is returned by Install on non-Linux platforms
// where the TLS kernel offload is unavailable. Callers fall back to
// userspace TLS.
var ErrKTLSUnavailable = errors.New("tlsutil: kTLS unavailable on this platform")

// Cipher mirrors the typed enum from the ktls build for API stability.
type Cipher uint8

const (
	CipherAES128GCM Cipher = iota
	CipherAES256GCM
)

// HashFor maps a cipher to the HKDF hash used to derive its keys. This
// stub version lets callers do key derivation even without kTLS —
// useful for tests.
func (c Cipher) HashFor() HashID {
	if c == CipherAES256GCM {
		return HashSHA384
	}
	return HashSHA256
}

// KeyLen returns the AEAD key length in bytes.
func (c Cipher) KeyLen() int {
	if c == CipherAES256GCM {
		return 32
	}
	return 16
}

// Install is a no-op stub that always fails. Use CipherFromSuite +
// HKDFExpandLabel directly if you need the keys for test purposes.
func Install(fd int, c Cipher, secrets TrafficSecrets) error {
	return ErrKTLSUnavailable
}

// CipherFromSuite mirrors the ktls-build function for API stability.
func CipherFromSuite(id uint16) (Cipher, bool) {
	switch id {
	case 0x1301:
		return CipherAES128GCM, true
	case 0x1302:
		return CipherAES256GCM, true
	}
	return 0, false
}
