// Standalone smoke test: run a TLS 1.3 handshake on a local TCP
// listener, capture the traffic secrets, and invoke tlsutil.Install on
// the server side. We don't try to drive application traffic through
// kTLS here — the goal is just to prove that:
//
//   1. Our NSS-log parsing gets both CLIENT_TRAFFIC_SECRET_0 and
//      SERVER_TRAFFIC_SECRET_0.
//   2. setsockopt(TCP_ULP=tls) is accepted by the kernel.
//   3. setsockopt(TLS_TX/TLS_RX, ...) is accepted by the kernel.
//
// Build: GOOS=linux go build -tags ktls -o ktls_smoke ./tlsutil/ktls_smoke
// Run on a Linux host with kernel ≥ 4.13 (we need the full kTLS ABI).
//
// This file is behind the `ktls` build tag for consistency with the
// rest of the ktls machinery — but since we call tlsutil.Install
// directly, the stub build of tlsutil would return ErrKTLSUnavailable
// and produce a meaningful smoke failure instead of a link error.

//go:build linux && ktls

package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"

	"tachyon/tlsutil"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("OK — kTLS setsockopt accepted for both TX and RX")
}

func run() error {
	cert, err := selfSigned()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() { done <- serverOnce(ln, cert) }()

	if err := clientOnce(ln.Addr().String()); err != nil {
		return fmt.Errorf("client: %w", err)
	}
	if err := <-done; err != nil {
		return fmt.Errorf("server: %w", err)
	}
	return nil
}

func serverOnce(ln net.Listener, cert tls.Certificate) error {
	raw, err := ln.Accept()
	if err != nil {
		return err
	}
	cap := tlsutil.NewCapture()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		KeyLogWriter: cap,
	}
	tc := tls.Server(raw, cfg)
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	// Read one byte to force the TLS 1.3 Finished exchange to fully
	// complete on both sides, which guarantees both application
	// traffic secrets have been logged.
	buf := make([]byte, 1)
	if _, err := tc.Read(buf); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	secrets, err := cap.Secrets()
	if err != nil {
		return err
	}
	cipher, ok := tlsutil.CipherFromSuite(tc.ConnectionState().CipherSuite)
	if !ok {
		return fmt.Errorf("unsupported suite 0x%x", tc.ConnectionState().CipherSuite)
	}

	// Pull the raw fd. tls.Conn.NetConn() returns the underlying
	// net.Conn; for TCP that's *net.TCPConn, which has File() to dup
	// out the fd.
	nc := tc.NetConn()
	tcpConn, ok := nc.(*net.TCPConn)
	if !ok {
		return fmt.Errorf("underlying conn is %T, not *net.TCPConn", nc)
	}
	f, err := tcpConn.File()
	if err != nil {
		return fmt.Errorf("dup fd: %w", err)
	}
	defer f.Close()
	fd := int(f.Fd())

	if err := tlsutil.Install(fd, cipher, secrets); err != nil {
		if errno, ok := err.(syscall.Errno); ok {
			return fmt.Errorf("install: %w (errno=%d)", err, int(errno))
		}
		return fmt.Errorf("install: %w", err)
	}
	fmt.Fprintf(os.Stderr, "installed: cipher=%d tx_key_len=%d\n", cipher, cipher.KeyLen())
	// Close tc so the client's Read returns and it exits cleanly.
	_ = tc.Close()
	return nil
}

func clientOnce(addr string) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
	})
	if err != nil {
		return err
	}
	// Send one byte to unblock the server's Read, then block on a
	// Read of our own so the conn stays open while the server calls
	// setsockopt. Without this, the client can close the socket
	// before TCP_ULP=tls reaches the kernel, producing ENOTCONN.
	if _, err := conn.Write([]byte{0x42}); err != nil {
		return err
	}
	// Wait for EOF from the server; any error here is fine — we just
	// need the conn alive through Install.
	_, _ = conn.Read(make([]byte, 1))
	_ = conn.Close()
	return nil
}

func selfSigned() (tls.Certificate, error) {
	// Delegate to tlsutil's helper via a tiny one-time listener dance
	// — we can't reach the package-private generateSelfSigned from
	// here, so write the cert to a temp dir instead.
	cdir, err := os.MkdirTemp("", "ktls_smoke")
	if err != nil {
		return tls.Certificate{}, err
	}
	certPath := cdir + "/cert.pem"
	keyPath := cdir + "/key.pem"
	if err := tlsutil.WriteSelfSigned(certPath, keyPath); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}
