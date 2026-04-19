package runtime

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"tachyon/internal/proxy"
)

// Worker wraps a listener and a proxy handler. Serve accepts in a tight loop
// and spawns one goroutine per connection. Phase 0 uses goroutines so we can
// get end-to-end correctness quickly; Phase 2 collapses to a single event
// loop driven by io_uring CQEs.
//
// A sync.WaitGroup tracks in-flight dispatches so Shutdown can drain them
// to completion after the listener closes. Callers that don't care about
// drain simply let the context expire and ignore the WG.
type Worker struct {
	Listener net.Listener
	Handler  *proxy.Handler
	Log      *slog.Logger

	// Dispatch, if non-nil, replaces the default `Handler.ServeConn(c)`
	// dispatch for each accepted connection. The TLS listener sets this
	// so it can inspect ALPN and fan H2 / H1 into the right server.
	Dispatch func(net.Conn)

	wg sync.WaitGroup
}

// Serve blocks until ctx is done, accepting connections and dispatching.
//
// Serve does not wait for in-flight dispatches. Call Drain after Serve
// returns to wait for them, bounded by the drain deadline.
func (w *Worker) Serve(ctx context.Context) error {
	// Close the listener when the context expires so Accept returns.
	go func() {
		<-ctx.Done()
		_ = w.Listener.Close()
	}()

	for {
		c, err := w.Listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Temporary errors (per net.Error deprecated but still surfaced):
			// sleep briefly and keep serving. Everything else is fatal.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		// Tune the client socket. NoDelay matters for latency; KeepAlive
		// lets us detect dead peers without a per-conn reaper goroutine.
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		w.wg.Add(1)
		if w.Dispatch != nil {
			go func(c net.Conn) {
				defer w.wg.Done()
				w.Dispatch(c)
			}(c)
		} else {
			go func(c net.Conn) {
				defer w.wg.Done()
				w.Handler.ServeConn(c)
			}(c)
		}
	}
}

// Drain waits for in-flight dispatches to finish, bounded by deadline.
// Returns true if all drained within the deadline, false on timeout.
//
// Typical use:
//
//	ctx, _ := signal.NotifyContext(...)
//	err := w.Serve(ctx)
//	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	w.Drain(drainCtx)
//
// Drain does not force-close client conns. Operators who need a hard
// deadline should call the process-level cancel (send a second signal,
// which the entry point turns into os.Exit(130)).
func (w *Worker) Drain(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
