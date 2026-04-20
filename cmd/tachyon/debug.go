package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"strings"
	"time"

	"tachyon/metrics"
)

// startDebugServer spins up the loopback-only debug HTTP endpoint.
// Returns nil (no server) if addr is empty. Returns an error if addr
// is non-empty but not on the 127.0.0.1 / localhost / ::1 loopback —
// we refuse to bind a pprof endpoint to a reachable address because
// the goroutine dumps and CPU profiles are a juicy target.
//
// The server runs in a goroutine; on ctx cancellation it triggers a
// graceful Shutdown with a short deadline. The caller is not expected
// to wait — main will exit on worker drain regardless.
func startDebugServer(ctx context.Context, addr string, log *slog.Logger) error {
	if addr == "" {
		return nil
	}
	if !isLoopbackAddr(addr) {
		return errors.New("debug-addr must bind to loopback (127.0.0.1, ::1, or localhost): got " + addr)
	}

	mux := http.NewServeMux()
	// /debug/pprof/* and children, via net/http/pprof's init registering
	// on DefaultServeMux. We delegate to the default mux so we don't
	// duplicate the pprof handler wiring.
	mux.Handle("/debug/", http.DefaultServeMux)
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = metrics.WritePrometheus(w)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("debug serve", "err", err)
		}
	}()
	// Shut the debug server down when the main worker context ends.
	// 1s is generous — /debug/pprof profiles can run for 30s, but
	// we're exiting the process anyway.
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	return nil
}

// isLoopbackAddr vets the bind address. Accepts "host:port" forms where
// host is 127.0.0.1, ::1, or localhost (any case). A missing host
// ("" before the colon, as in ":9000") is rejected — we require an
// explicit loopback to prevent accidental 0.0.0.0 binds.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.ToLower(host)
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}
