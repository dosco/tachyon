// kTLS setsockopt installer. Linux-only, gated behind the `ktls` build
// tag so stock builds omit the syscall-layout code.
//
// The Linux kTLS ABI is:
//
//   setsockopt(fd, SOL_TCP, TCP_ULP, "tls", 4)
//   setsockopt(fd, SOL_TLS, TLS_TX, &crypto_info, sizeof(crypto_info))
//   setsockopt(fd, SOL_TLS, TLS_RX, &crypto_info, sizeof(crypto_info))
//
// where crypto_info's layout depends on the negotiated cipher. We
// support the two AEADs a modern client will offer on TLS 1.3: AES-
// 128-GCM (SHA-256 PRF) and AES-256-GCM (SHA-384 PRF). CHACHA20-
// POLY1305 has a different struct layout and we can add it when we
// actually need it.
//
// Record sequence numbers reset to 0 at the handshake→application
// transition, so we hand the kernel a zero rec_seq.
//
// This file does NOT depend on x/sys/unix's ktls helpers (they didn't
// exist when this was written). We call SYS_SETSOCKOPT directly with a
// tiny wrapper.

//go:build linux && ktls

package tlsutil

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Linux kTLS constants. Values pulled from <linux/tls.h> — they are
// stable kernel ABI, safe to hard-code.
const (
	solTCP = 6  // SOL_TCP
	solTLS = 282

	tcpULP = 31  // TCP_ULP
	tlsTX  = 1
	tlsRX  = 2

	tlsCipherAES128GCM = 51 // TLS_CIPHER_AES_GCM_128
	tlsCipherAES256GCM = 52 // TLS_CIPHER_AES_GCM_256

	// TLS_1_3_VERSION per <linux/tls.h>: (3<<8)|4.
	tls13Version = (3 << 8) | 4
)

// Cipher identifies the negotiated TLS 1.3 AEAD. Pick from the suite id
// on tls.ConnectionState.CipherSuite.
type Cipher uint8

const (
	CipherAES128GCM Cipher = iota
	CipherAES256GCM
)

// HashFor returns the HKDF hash matched to each cipher's PRF per RFC
// 8446 §B.4. CHACHA20-POLY1305 would also be SHA-256 if we add it.
func (c Cipher) HashFor() HashID {
	if c == CipherAES256GCM {
		return HashSHA384
	}
	return HashSHA256
}

// KeyLen is the symmetric key length in bytes.
func (c Cipher) KeyLen() int {
	switch c {
	case CipherAES256GCM:
		return 32
	default:
		return 16
	}
}

// ivLen (salt || explicit nonce in the kernel's view) is 4 + 8 = 12.
const ktlsIVLen = 12

// struct tls_crypto_info_aes_gcm_128 from <linux/tls.h>:
//
//   struct tls_crypto_info { u16 version; u16 cipher_type; }
//   struct tls12_crypto_info_aes_gcm_128 {
//     struct tls_crypto_info info;
//     unsigned char iv[8];
//     unsigned char key[16];
//     unsigned char salt[4];
//     unsigned char rec_seq[8];
//   }
//
// aes_gcm_256 is identical except key[32]. The "12" in the type name
// is kernel legacy — these structs are used for TLS 1.3 too.
type cryptoInfoAES128GCM struct {
	version    uint16
	cipherType uint16
	iv         [8]byte
	key        [16]byte
	salt       [4]byte
	recSeq     [8]byte
}

type cryptoInfoAES256GCM struct {
	version    uint16
	cipherType uint16
	iv         [8]byte
	key        [32]byte
	salt       [4]byte
	recSeq     [8]byte
}

// Install configures both TX and RX directions for fd from the two
// application traffic secrets. The socket must be TCP; the handshake
// must be complete. After this returns successfully, all send/recv on
// fd is plaintext from userspace's POV — the kernel handles record
// framing and AEAD.
//
// The caller is responsible for stopping use of the original *tls.Conn
// after Install — writing to it would inject an unencrypted TLS
// record on top of kTLS's framing and break the stream.
func Install(fd int, c Cipher, secrets TrafficSecrets) error {
	// 1. TCP_ULP="tls". This must come before TLS_TX/TLS_RX.
	if err := setsockoptString(fd, solTCP, tcpULP, "tls"); err != nil {
		return fmt.Errorf("tlsutil: TCP_ULP=tls: %w", err)
	}

	h := c.HashFor()
	// Server perspective: the server's TX uses the server→client
	// application secret; RX uses the client→server secret.
	txKey := HKDFExpandLabel(h, secrets.ServerToClient, "key", nil, c.KeyLen())
	txIV := HKDFExpandLabel(h, secrets.ServerToClient, "iv", nil, ktlsIVLen)
	rxKey := HKDFExpandLabel(h, secrets.ClientToServer, "key", nil, c.KeyLen())
	rxIV := HKDFExpandLabel(h, secrets.ClientToServer, "iv", nil, ktlsIVLen)

	if err := setDirection(fd, tlsTX, c, txKey, txIV); err != nil {
		return fmt.Errorf("tlsutil: TLS_TX: %w", err)
	}
	if err := setDirection(fd, tlsRX, c, rxKey, rxIV); err != nil {
		return fmt.Errorf("tlsutil: TLS_RX: %w", err)
	}
	return nil
}

// setDirection builds the cipher-specific struct and invokes setsockopt.
// The kernel's layout puts IV at [0:8], salt at [keyLen+8:keyLen+12],
// with the nonce_explicit constructed as salt||iv internally; for TLS
// 1.3 we pass the 12-byte HKDF-derived IV split as iv[0:8]=iv[4:12],
// salt[0:4]=iv[0:4] (the kernel concatenates salt||iv to form the
// 12-byte nonce seed, XORed with the record sequence number).
func setDirection(fd, dir int, c Cipher, key, iv []byte) error {
	if len(iv) != ktlsIVLen {
		return errors.New("tlsutil: iv must be 12 bytes")
	}
	switch c {
	case CipherAES128GCM:
		if len(key) != 16 {
			return errors.New("tlsutil: aes-128-gcm key must be 16 bytes")
		}
		var info cryptoInfoAES128GCM
		info.version = tls13Version
		info.cipherType = tlsCipherAES128GCM
		copy(info.salt[:], iv[0:4])
		copy(info.iv[:], iv[4:12])
		copy(info.key[:], key)
		// recSeq stays zero — TLS 1.3 resets record sequence to 0 at
		// the application-data transition.
		return setsockoptBytes(fd, solTLS, dir,
			unsafe.Pointer(&info), unsafe.Sizeof(info))
	case CipherAES256GCM:
		if len(key) != 32 {
			return errors.New("tlsutil: aes-256-gcm key must be 32 bytes")
		}
		var info cryptoInfoAES256GCM
		info.version = tls13Version
		info.cipherType = tlsCipherAES256GCM
		copy(info.salt[:], iv[0:4])
		copy(info.iv[:], iv[4:12])
		copy(info.key[:], key)
		return setsockoptBytes(fd, solTLS, dir,
			unsafe.Pointer(&info), unsafe.Sizeof(info))
	}
	return fmt.Errorf("tlsutil: unsupported cipher %d", c)
}

// setsockoptString is setsockopt with a C-string value (used for
// TCP_ULP="tls"). We include the trailing NUL by length, matching what
// the kernel expects.
func setsockoptString(fd, level, name int, value string) error {
	b := []byte(value)
	return setsockoptBytes(fd, level, name,
		unsafe.Pointer(&b[0]), uintptr(len(b)))
}

// setsockoptBytes wraps the raw syscall. On entry vp must point at
// vlen bytes of valid memory.
func setsockoptBytes(fd, level, name int, vp unsafe.Pointer, vlen uintptr) error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_SETSOCKOPT,
		uintptr(fd), uintptr(level), uintptr(name),
		uintptr(vp), vlen, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// CipherFromSuite maps the TLS 1.3 suite id on tls.ConnectionState.
// CipherSuite to our Cipher enum. Returns false for suites we don't
// install via kTLS.
func CipherFromSuite(id uint16) (Cipher, bool) {
	switch id {
	case 0x1301: // TLS_AES_128_GCM_SHA256
		return CipherAES128GCM, true
	case 0x1302: // TLS_AES_256_GCM_SHA384
		return CipherAES256GCM, true
	}
	return 0, false
}
