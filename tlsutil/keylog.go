// Per-connection capture of TLS 1.3 application traffic secrets via
// crypto/tls's KeyLogWriter.
//
// Why this file exists:
//   kTLS (TCP_ULP=tls + setsockopt TLS_TX/RX) needs the AEAD key + IV
//   for each direction after the handshake finishes. Those are derived
//   from the "application traffic secrets" (RFC 8446 §7.1). crypto/tls
//   does not expose those secrets on *tls.Conn or tls.ConnectionState,
//   but it *does* expose them via tls.Config.KeyLogWriter in the NSS
//   key-log format, the same format Wireshark consumes.
//
// The NSS format is one secret per line:
//
//   <LABEL> <CLIENT_RANDOM_hex> <SECRET_hex>
//
// crypto/tls calls KeyLogWriter.Write once per label. A Config-wide
// KeyLogWriter would interleave lines from concurrent handshakes and
// force us to demultiplex by client_random — but ClientHelloInfo does
// not expose Random, so we can't register a per-conn bucket from the
// GetConfigForClient callback.
//
// Simpler path: clone the Config on every accept and install a fresh
// per-conn *Capture as that clone's KeyLogWriter. Config.Clone is a
// shallow copy of a handful of fields — negligible on the handshake
// path, and it cleanly isolates each connection's secrets.
//
// Security note: these secrets are highly sensitive — with them an
// attacker decrypts every application byte. The captures live in RAM,
// bound to the life of a single connection, and are zeroed on Release.

package tlsutil

import (
	"crypto/tls"
	"encoding/hex"
	"errors"
	"sync"
)

// TrafficSecrets holds the two TLS 1.3 application traffic secrets.
// Length matches the negotiated hash output (32 for SHA-256, 48 for
// SHA-384). ClientToServer feeds the server's *receive* (TLS_RX) key
// derivation; ServerToClient feeds the server's *send* (TLS_TX) key.
type TrafficSecrets struct {
	ClientToServer []byte
	ServerToClient []byte
}

// Zero wipes the secrets. Call once they're no longer needed.
func (t *TrafficSecrets) Zero() {
	for i := range t.ClientToServer {
		t.ClientToServer[i] = 0
	}
	for i := range t.ServerToClient {
		t.ServerToClient[i] = 0
	}
}

// Capture is an io.Writer that crypto/tls funnels NSS key-log lines
// through. Install one per *tls.Conn by cloning the base Config and
// assigning the clone's KeyLogWriter to a fresh *Capture.
type Capture struct {
	mu     sync.Mutex
	client []byte // CLIENT_TRAFFIC_SECRET_0 secret
	server []byte // SERVER_TRAFFIC_SECRET_0 secret
}

// NewCapture returns an empty capture ready to install on a Config.
func NewCapture() *Capture { return &Capture{} }

// Write implements io.Writer. crypto/tls calls this exactly once per
// secret event with a single complete newline-terminated NSS line.
// Unrecognized labels (EXPORTER_SECRET etc.) are accepted and ignored
// so we never stall the handshake by returning an error.
func (c *Capture) Write(p []byte) (int, error) {
	line := p
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	label, rest := splitSpace(line)
	_, secretHex := splitSpace(rest) // middle token is client_random; unused here
	if len(label) == 0 || len(secretHex) == 0 {
		return len(p), nil
	}
	secret := make([]byte, hex.DecodedLen(len(secretHex)))
	n, err := hex.Decode(secret, secretHex)
	if err != nil {
		return len(p), nil
	}
	secret = secret[:n]
	c.mu.Lock()
	switch string(label) {
	case "CLIENT_TRAFFIC_SECRET_0":
		c.client = secret
	case "SERVER_TRAFFIC_SECRET_0":
		c.server = secret
	}
	c.mu.Unlock()
	return len(p), nil
}

// ErrNoSecrets means the handshake finished but the expected TLS 1.3
// application traffic secrets never arrived. Caller should fall back
// to userspace TLS (or close the conn, per policy).
var ErrNoSecrets = errors.New("tlsutil: no TLS 1.3 traffic secrets captured")

// Secrets returns both application traffic secrets. The caller invokes
// this *after* tls.Conn.Handshake() completes — by then both
// CLIENT_TRAFFIC_SECRET_0 and SERVER_TRAFFIC_SECRET_0 have been logged
// (crypto/tls emits both during the handshake). If either is missing
// the negotiated version wasn't TLS 1.3 and kTLS is not available.
func (c *Capture) Secrets() (TrafficSecrets, error) {
	c.mu.Lock()
	cs := c.client
	ss := c.server
	c.mu.Unlock()
	if len(cs) == 0 || len(ss) == 0 {
		return TrafficSecrets{}, ErrNoSecrets
	}
	return TrafficSecrets{ClientToServer: cs, ServerToClient: ss}, nil
}

// CloneConfigWithCapture returns a shallow copy of cfg with its
// KeyLogWriter set to a fresh *Capture, suitable for exactly one
// accepted connection. The returned Capture is how the caller later
// retrieves the per-conn traffic secrets.
func CloneConfigWithCapture(cfg *tls.Config) (*tls.Config, *Capture) {
	cap := NewCapture()
	cc := cfg.Clone()
	cc.KeyLogWriter = cap
	return cc, cap
}

// splitSpace splits on the first ASCII space. No allocations.
func splitSpace(b []byte) (head, tail []byte) {
	for i, c := range b {
		if c == ' ' {
			return b[:i], b[i+1:]
		}
	}
	return b, nil
}
