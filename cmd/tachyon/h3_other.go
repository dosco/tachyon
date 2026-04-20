//go:build !linux

package main

import (
	"context"
	"log/slog"

	"tachyon/internal/proxy"
	"tachyon/quic"
)

// startH3 is a stub on non-Linux; the QUIC endpoint can still be
// created but no HTTP/3 handler is attached (the proxy handlers
// themselves are Linux-only).
func startH3(ctx context.Context, ep *quic.Endpoint, h *proxy.Handler, log *slog.Logger) {
	_ = ctx
	_ = h
	go func() {
		if err := ep.Serve(ctx); err != nil {
			log.Error("quic serve", "err", err)
		}
	}()
}
