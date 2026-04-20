package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// installShutdownSignals returns:
//
//   - a context that cancels on the first SIGINT or SIGTERM (the "begin
//     drain" signal), and
//   - a stop function that releases the signal handler.
//
// A second matching signal force-exits with code 130 so an operator can
// abort a stuck drain.
func installShutdownSignals(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)

	// Arm the second-signal force-exit on a private channel. This has to
	// live alongside NotifyContext because the latter only cancels on the
	// first signal — subsequent signals are swallowed by its internal
	// handler. We want a second Ctrl-C to hard-kill.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		// Wait for the first signal — NotifyContext also receives this,
		// so the ctx gets cancelled in parallel.
		<-ch
		// The next one is the abort.
		<-ch
		os.Exit(130)
	}()

	return ctx, stop
}

