package main

import (
	"crypto/tls"

	"tachyon/internal/router"
	"tachyon/tlsutil"
)

// buildQUICTLSConfig builds the *tls.Config handed to the QUIC endpoint.
// It reuses the same cert/key the TCP TLS listener uses (or falls back
// to the dedicated quic.cert/quic.key from the intent grammar) and
// forces NextProtos to ["h3"] for ALPN. Session tickets are disabled
// because the QUIC stack does not yet implement TLS session storage.
func buildQUICTLSConfig(cfg *router.QUICConfig) (*tls.Config, error) {
	if len(cfg.ALPN) == 0 {
		cfg.ALPN = []string{"h3"}
	}
	base, err := tlsutil.NewServerConfig(tlsutil.ServerOptions{
		CertFile:   cfg.Cert,
		KeyFile:    cfg.Key,
		NextProtos: cfg.ALPN,
	})
	if err != nil {
		return nil, err
	}
	base.SessionTicketsDisabled = true
	base.MinVersion = tls.VersionTLS13
	return base, nil
}
