//go:build !linux

package main

import (
	"errors"
	"log/slog"

	"tachyon/internal/proxy"
	"tachyon/internal/router"
	trt "tachyon/internal/runtime"
)

// tlsReloader is a stub on non-Linux; the real type lives behind
// `//go:build linux` in tls.go. It exists here so main.go compiles on
// all platforms (with the TLS path unreachable at runtime).
type tlsReloader struct{}

// Reload is a no-op stub.
func (*tlsReloader) Reload(string, string) error { return nil }

// startTLSWorker is Linux-only: the TLS + kTLS fast path depends on the
// Linux kernel's TLS ULP and SO_REUSEPORT semantics. On other platforms
// we surface a clear error so it is obvious what to deploy on.
func startTLSWorker(cfg *router.TLSConfig, h *proxy.Handler, log *slog.Logger, idx int) (*trt.Worker, *tlsReloader, error) {
	return nil, nil, errors.New("tls listener is linux-only")
}
