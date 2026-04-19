// Fallback stub for builds without the `ktls` build tag (and for
// non-Linux builds of the package's syntactic compile). Keeps the
// public API surface stable so callers don't need their own build tag.

//go:build !linux || !ktls

package tlsutil

import "errors"

// ErrKTLSUnavailable is returned by Install when the binary was built
// without the `ktls` tag (or is running on a non-Linux platform).
var ErrKTLSUnavailable = errors.New("tlsutil: kTLS not compiled in (build with -tags ktls)")

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
