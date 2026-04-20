//go:build !linux

package main

import (
	"errors"
	"log/slog"

	irt "tachyon/internal/intent/runtime"
	"tachyon/internal/router"
)

func runUring(cfg *router.Config, intents irt.RoutePrograms, log *slog.Logger, sqpoll bool, spliceMin int64) error {
	_ = cfg
	_ = intents
	_ = log
	_ = sqpoll
	_ = spliceMin
	return errors.New("uring worker is linux-only")
}
