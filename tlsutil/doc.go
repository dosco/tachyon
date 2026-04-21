// Package tlsutil wraps crypto/tls for tachyon.
//
// # Status
//
// Phase 4 scaffolding. crypto/tls is already decent on modern Go; this
// package exists to:
//
//  1. Own the *tls.Config construction (ciphers, session tickets, min
//     version, ALPN "h2","http/1.1").
//  2. Rotate session ticket keys on a timer so resumption stays cheap
//     without pinning forever.
//  3. Bridge a tls.Conn onto an io_uring-driven socket (conn_bridge.go).
//  4. kTLS on Linux: after a successful handshake, hand the raw fd to
//     the kernel and subsequent writes use SEND_ZC. Default on Linux;
//     stubbed on other platforms.
//
// # Layout
//
//   - server.go        - Config builder, ticket key rotation, async OCSP
//   - conn_bridge.go   - in-memory net.Conn over iouring recv/send
//   - ktls_linux.go    - TCP_ULP + TLS_TX/RX setsockopt (Linux)
//   - ktls_stub.go     - no-op Install for non-Linux builds
package tlsutil
