//go:build linux

package main

import (
	"log/slog"

	irt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
	"tachyon/internal/runtime/uring"
)

func runUring(cfg *router.Config, intents irt.RoutePrograms, log *slog.Logger, sqpoll bool, spliceMin int64) error {
	defs := make([]uring.UpstreamDef, 0, len(cfg.Upstreams))
	for name, u := range cfg.Upstreams {
		defs = append(defs, uring.UpstreamDef{
			Name:        name,
			Addrs:       u.Addrs,
			IdlePerHost: u.IdlePerHost,
		})
	}
	s := &uring.Server{
		Router:        router.New(cfg.Routes),
		Intents:       intents,
		IntentState:   irt.NewState(),
		Upstreams:     defs,
		Log:           log,
		SQPoll:        sqpoll,
		SpliceMinBody: spliceMin,
	}
	return s.Serve(cfg.Listen)
}
