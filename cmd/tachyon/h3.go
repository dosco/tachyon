//go:build linux

package main

import (
	"context"
	"log/slog"

	"tachyon/http3"
	"tachyon/internal/proxy"
	"tachyon/quic"
)

// startH3 spawns the QUIC accept loop and dispatches each connection
// to the HTTP/3 server, with requests bridged into the shared H1/H2
// proxy handler. Linux-only because it depends on the Linux-tagged
// proxy handlers.
func startH3(ctx context.Context, ep *quic.Endpoint, h *proxy.Handler, log *slog.Logger) {
	h3h := proxy.NewH3Handler(h)
	if port := ep.Port(); port != "" {
		h.SetAltSvc(`h3=":` + port + `"; ma=86400`)
	}
	go func() {
		if err := ep.Serve(ctx); err != nil {
			log.Error("quic serve", "err", err)
		}
	}()
	go func() {
		for {
			conn, err := ep.AcceptConn(ctx)
			if err != nil {
				return
			}
			go func() {
				if err := http3.ServeConn(ctx, conn, h3h.Handle); err != nil {
					log.Debug("http3 serve", "err", err)
				}
			}()
		}
	}()
}
