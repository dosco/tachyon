// Server-side *tls.Config construction.
//
// Why this file exists: crypto/tls is good out of the box but a handful of
// knobs make the difference between "functional" and "fast on a bench":
//
//   - Session tickets (PSK resumption) with a rotating key.
//   - Modern-only cipher suites; TLS 1.2 floor, 1.3 preferred.
//   - ALPN advertising http/1.1 (h2 added in Phase 5).
//   - SessionTicketsDisabled=false (it defaults to false but we're explicit).
//
// Ticket-key rotation runs on a time.Timer; on key switch the previous key
// stays in the accepted-list for one rotation so resumption doesn't cliff.
//
// OCSP stapling is left to Phase 4+: it needs a network fetch and isn't on
// the hot path. We ship with a nil OCSPStaple and revisit when benches
// demand it.

package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"time"
)

// ServerOptions configures a server tls.Config.
type ServerOptions struct {
	// CertFile and KeyFile are PEM paths. If both empty, a self-signed
	// cert is generated in memory — useful for benches, not production.
	CertFile string
	KeyFile  string

	// TicketRotate: interval between session ticket key rotations. Zero
	// means no rotation (a single random key for the process lifetime).
	TicketRotate time.Duration

	// NextProtos. Default []string{"http/1.1"}. Phase 5 adds "h2".
	NextProtos []string
}

// NewServerConfig builds a *tls.Config ready to pass to tls.NewListener.
// It also kicks off any background goroutines needed for key rotation;
// those tie their lifetime to the returned Config's underlying state,
// which lives as long as the listener keeps the Config referenced.
func NewServerConfig(opt ServerOptions) (*tls.Config, error) {
	cert, err := loadOrGenerateCert(opt.CertFile, opt.KeyFile)
	if err != nil {
		return nil, err
	}
	cfg := baseConfig(opt)
	cfg.Certificates = []tls.Certificate{cert}
	if err := maybeStartRotator(cfg, opt); err != nil {
		return nil, err
	}
	return cfg, nil
}

// NewServerConfigWithGetCert builds a Config that resolves the server
// certificate via the supplied GetCertificate hook on every handshake.
// This is the form the proxy uses in production so SIGHUP-driven cert
// reload can atomically swap the cert without rebuilding the Config.
//
// opt.CertFile and opt.KeyFile are ignored here; the caller is expected
// to manage the cert lifecycle behind getCert.
func NewServerConfigWithGetCert(opt ServerOptions, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*tls.Config, error) {
	if getCert == nil {
		return nil, fmt.Errorf("tlsutil: GetCertificate hook is required")
	}
	cfg := baseConfig(opt)
	cfg.GetCertificate = getCert
	if err := maybeStartRotator(cfg, opt); err != nil {
		return nil, err
	}
	return cfg, nil
}

// baseConfig populates the fields shared by NewServerConfig and
// NewServerConfigWithGetCert. The caller supplies Certificates (static)
// or GetCertificate (hook).
func baseConfig(opt ServerOptions) *tls.Config {
	np := opt.NextProtos
	if len(np) == 0 {
		np = []string{"http/1.1"}
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Let the stdlib pick TLS 1.3 when possible. CipherSuites here
		// only affect TLS 1.2; 1.3 suites are hard-coded in stdlib.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
		},
		PreferServerCipherSuites: true,
		NextProtos:               np,
	}
}

// maybeStartRotator installs a rotating session ticket key on cfg if
// opt.TicketRotate > 0.
func maybeStartRotator(cfg *tls.Config, opt ServerOptions) error {
	if opt.TicketRotate <= 0 {
		return nil
	}
	kr := &keyRotator{cfg: cfg, interval: opt.TicketRotate}
	if err := kr.seed(); err != nil {
		return err
	}
	go kr.loop()
	return nil
}

// keyRotator owns the rotating session-ticket keys and pokes them into the
// tls.Config. SetSessionTicketKeys accepts a list; the first is used for
// new tickets, the rest are still accepted for resumption — so we keep the
// previous key alive for one cycle to avoid cliffs on rotation.
type keyRotator struct {
	cfg      *tls.Config
	interval time.Duration

	mu       sync.Mutex
	current  [32]byte
	previous [32]byte
	hasPrev  bool
}

func (k *keyRotator) seed() error {
	if _, err := io.ReadFull(rand.Reader, k.current[:]); err != nil {
		return fmt.Errorf("tlsutil: seed ticket key: %w", err)
	}
	k.cfg.SetSessionTicketKeys([][32]byte{k.current})
	return nil
}

func (k *keyRotator) loop() {
	t := time.NewTicker(k.interval)
	defer t.Stop()
	for range t.C {
		k.mu.Lock()
		k.previous = k.current
		k.hasPrev = true
		if _, err := io.ReadFull(rand.Reader, k.current[:]); err != nil {
			k.mu.Unlock()
			continue
		}
		keys := [][32]byte{k.current, k.previous}
		k.cfg.SetSessionTicketKeys(keys)
		k.mu.Unlock()
	}
}

// loadOrGenerateCert: either read from disk or mint a self-signed P-256
// cert valid for 24h. Only the bench path uses in-memory certs.
func loadOrGenerateCert(certFile, keyFile string) (tls.Certificate, error) {
	if certFile != "" && keyFile != "" {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	if (certFile == "") != (keyFile == "") {
		return tls.Certificate{}, fmt.Errorf("tlsutil: need both cert and key, or neither")
	}
	return generateSelfSigned()
}

// GenerateSelfSigned mints a self-signed P-256 cert valid for 24h. Used
// by the bench path and by the tls-reload logic's fallback when no cert
// path is configured.
func GenerateSelfSigned() (tls.Certificate, error) { return generateSelfSigned() }

func generateSelfSigned() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "tachyon-bench"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "tachyon-bench"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// WriteSelfSigned generates a cert+key pair and writes them to the given
// paths. Used by bench scaffolding so curl/wrk2 can trust --cacert the file.
func WriteSelfSigned(certPath, keyPath string) error {
	c, err := generateSelfSigned()
	if err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: c.Certificate[0],
	}), 0o644); err != nil {
		return err
	}
	keyBytes, err := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: keyBytes,
	}), 0o600)
}
