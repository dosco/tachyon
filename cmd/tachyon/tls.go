//go:build linux

package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"tachyon/http2"
	"tachyon/internal/proxy"
	"tachyon/internal/router"
	trt "tachyon/internal/runtime"
	"tachyon/tlsutil"
)

// tlsReloader holds the cert currently presented on the TLS listener.
// SIGHUP reload re-reads the PEM files, parses, and atomically swaps the
// cert. On parse error the old cert stays installed.
//
// We install the cert via tls.Config.GetCertificate rather than the
// static Certificates slice so the tls runtime asks us for the cert on
// every handshake; the atomic load on each handshake is free.
type tlsReloader struct {
	cert atomic.Pointer[tls.Certificate]
}

// get returns the current cert for the stdlib TLS runtime.
func (r *tlsReloader) get(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := r.cert.Load()
	if c == nil {
		return nil, fmt.Errorf("tls: no certificate installed")
	}
	return c, nil
}

// Reload re-reads the cert and key PEM files and atomically replaces the
// cached cert. If either is empty, it generates a self-signed cert (bench
// behavior). Errors surface without touching the current cert.
func (r *tlsReloader) Reload(certFile, keyFile string) error {
	var cert tls.Certificate
	var err error
	if certFile != "" && keyFile != "" {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	} else if certFile == "" && keyFile == "" {
		cert, err = tlsutil.GenerateSelfSigned()
	} else {
		err = fmt.Errorf("tls: need both cert and key, or neither")
	}
	if err != nil {
		return err
	}
	r.cert.Store(&cert)
	return nil
}

// startTLSWorker binds f.tlsAddr with SO_REUSEPORT and returns a Worker
// whose Dispatch wraps each accepted TCP conn in a *tls.Conn with a
// per-connection KeyLogWriter capture. After the handshake we try to
// install kTLS on the socket; on success downstream code reads and
// writes plaintext while the kernel handles TLS framing.
//
// The per-accept Config.Clone is cheap (it's a shallow copy of a
// handful of fields) and is the only way stdlib tls exposes TLS 1.3
// application traffic secrets to us — the shared base Config can't
// demux per-conn NSS lines because ClientHelloInfo.Random isn't
// exported from crypto/tls.
//
// If the kernel doesn't support TLS_TX/TLS_RX (old kernel, unsupported
// cipher), tlsutil.Install returns an error and we fall through to
// userspace TLS. The wiring is identical either way.
//
// ALPN negotiates "h2" or "http/1.1". Both share router + pools with
// the plaintext handler.
//
// Returns the Worker and a tlsReloader handle for SIGHUP-driven cert
// rotation.
func startTLSWorker(cfg *router.TLSConfig, h *proxy.Handler, log *slog.Logger, idx int) (*trt.Worker, *tlsReloader, error) {
	reloader := &tlsReloader{}
	if err := reloader.Reload(cfg.Cert, cfg.Key); err != nil {
		return nil, nil, err
	}
	base, err := tlsutil.NewServerConfigWithGetCert(tlsutil.ServerOptions{
		TicketRotate:  12 * time.Hour,
		TicketKeySeed: readTicketSeed(),
		NextProtos:    []string{"h2", "http/1.1"},
	}, reloader.get)
	if err != nil {
		return nil, nil, err
	}
	ln, err := trt.Listen(cfg.Addr)
	if err != nil {
		return nil, nil, err
	}

	h2 := proxy.NewH2Handler(h)

	dispatch := func(raw net.Conn) {
		cfg, cap := tlsutil.CloneConfigWithCapture(base)
		// Go's crypto/tls sends one NewSessionTicket after the TLS 1.3
		// handshake, which bumps the server's write record sequence
		// to 1 before we can install kTLS. The kernel starts rec_seq
		// at 0, so the first post-handoff server response would be
		// decrypted by the peer with the wrong nonce and rejected as
		// a bad MAC. Disabling tickets here keeps the record counter
		// at 0 through the handoff. Trade-off: no PSK resumption for
		// this TLS listener; acceptable for the kTLS fast path.
		cfg.SessionTicketsDisabled = true
		tc := tls.Server(raw, cfg)
		if err := tc.Handshake(); err != nil {
			_ = tc.Close()
			return
		}

		state := tc.ConnectionState()
		alpn := state.NegotiatedProtocol

		// Try kTLS. Requires TLS 1.3 + a supported AEAD; otherwise we
		// silently fall through to userspace TLS.
		plainConn := serveOverKTLS(tc, state, cap, log)
		// Release the capture regardless of success — secrets live on
		// in tlsutil.Install's derived key memory if it succeeded.
		cap_ := cap // closure-local alias; avoid go vet "lostcancel"-style confusion
		_ = cap_

		conn := net.Conn(tc)
		if plainConn != nil {
			conn = plainConn
		}

		if alpn == "h2" {
			_ = http2.Serve(conn, h2)
			return
		}
		h.ServeConn(conn)
	}
	return &trt.Worker{Listener: ln, Handler: h, Log: log, Dispatch: dispatch}, reloader, nil
}

// serveOverKTLS tries to install kTLS on the connection's underlying
// socket. On success it returns the raw *net.TCPConn to hand to the
// protocol layer (reads + writes are now plaintext from userspace's
// POV — the kernel frames records). On any failure it returns nil and
// the caller should keep using the *tls.Conn.
//
// Assumptions enforced by construction:
//
//   - TLS 1.3 only. tlsutil.CipherFromSuite rejects anything else, so
//     we never try to install on a 1.2 connection.
//   - The *tls.Conn has no application data buffered internally at
//     this point. That holds for our use (no 0-RTT, the first app
//     byte comes after the server's Finished has been written).
func serveOverKTLS(tc *tls.Conn, state tls.ConnectionState, cap *tlsutil.Capture, log *slog.Logger) net.Conn {
	if state.Version != tls.VersionTLS13 {
		return nil
	}
	cipher, ok := tlsutil.CipherFromSuite(state.CipherSuite)
	if !ok {
		return nil
	}
	secrets, err := cap.Secrets()
	if err != nil {
		return nil
	}
	nc := tc.NetConn()
	tcpConn, ok := nc.(*net.TCPConn)
	if !ok {
		return nil
	}
	// SyscallConn lets us call setsockopt on the owned fd without
	// dup'ing it. Dup would leave two fds pointing at the same socket
	// and complicate close ordering.
	sc, err := tcpConn.SyscallConn()
	if err != nil {
		return nil
	}
	var installErr error
	ctrlErr := sc.Control(func(fd uintptr) {
		installErr = tlsutil.Install(int(fd), cipher, secrets)
	})
	if ctrlErr != nil || installErr != nil {
		// Log at debug level; on kernels that don't support TLS_TX
		// this fires on every handshake and would spam the log.
		if log != nil {
			log.Debug("ktls install skipped",
				"cipher", cipher, "ctrl_err", ctrlErr, "install_err", installErr)
		}
		return nil
	}
	// Scrub the captured secrets from memory — kernel has its own
	// copy now.
	secrets.Zero()
	return tcpConn
}
