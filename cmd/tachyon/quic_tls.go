package main

import (
	"crypto/tls"
	"time"

	"tachyon/internal/router"
	"tachyon/tlsutil"
)

// buildQUICTLSConfig builds the *tls.Config handed to the QUIC endpoint.
// It reuses the same cert/key the TCP TLS listener uses (or falls back
// to the dedicated quic.cert/quic.key from the intent grammar) and
// forces NextProtos to ["h3"] for ALPN.
//
// Session tickets are enabled: 1-RTT resumption works end-to-end through
// stdlib crypto/tls.QUICConn — the server emits a NewSessionTicket
// after handshake completion (see quic.connState.sendSessionTicket),
// the client presents it on a subsequent Initial, and the TLS state
// machine restores the master secret without a second certificate
// exchange. Pure latency win, no replay-safety concern because the
// resumed handshake still requires 1 RTT before the client may send
// application data.
//
// The ticket key itself is derived from a process-tree-shared seed
// (see ticket_seed.go) so clients resume on whichever SO_REUSEPORT
// worker the kernel hands them, not just the one that issued the
// original ticket.
//
// What's still off: 0-RTT / early-data. 0-RTT data is inherently
// replayable and needs an allow-list at the request level
// (idempotent methods only, per RFC 8470) plus a server-side strike
// register. Tracked as its own work item; the code here explicitly
// does NOT opt the session ticket into early-data eligibility.
func buildQUICTLSConfig(cfg *router.QUICConfig) (*tls.Config, error) {
	if len(cfg.ALPN) == 0 {
		cfg.ALPN = []string{"h3"}
	}
	base, err := tlsutil.NewServerConfig(tlsutil.ServerOptions{
		CertFile:      cfg.Cert,
		KeyFile:       cfg.Key,
		NextProtos:    cfg.ALPN,
		TicketRotate:  12 * time.Hour,
		TicketKeySeed: readTicketSeed(),
	})
	if err != nil {
		return nil, err
	}
	base.SessionTicketsDisabled = false
	base.MinVersion = tls.VersionTLS13
	return base, nil
}
