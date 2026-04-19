//go:build integration

// Integration harness for tachyon.
//
// Each test spins up (a) one or more in-process HTTP origins on
// ephemeral ports, (b) a tachyon *proxy.Handler wrapping them via a
// *runtime.Worker on another ephemeral port, and (c) a plain net/http
// client or raw net.Conn that hits the proxy.
//
// Keeping the harness in a separate package and build tag means the
// normal `go test ./...` command skips it; CI runs
// `go test -tags=integration ./e2e/...`.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"tachyon/internal/proxy"
	"tachyon/internal/router"
	trt "tachyon/internal/runtime"
	"tachyon/internal/upstream"
)

// testProxy is a started tachyon worker on an ephemeral port plus the
// goroutine machinery to stop it cleanly. Callers use ProxyAddr() to
// build request URLs.
type testProxy struct {
	t        *testing.T
	addr     string
	handler  *proxy.Handler
	worker   *trt.Worker
	cancel   context.CancelFunc
	serveErr chan error
}

// startProxy listens on 127.0.0.1:0 and serves a single-route config
// that forwards everything to originAddr. Returns a cleanup registered
// via t.Cleanup.
func startProxy(t *testing.T, originAddr string) *testProxy {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	r := router.New([]router.Rule{{Host: "*", Path: "/", Upstream: "default"}})
	h := proxy.NewHandler(r, upstream.NewPools(defs))

	w := &trt.Worker{
		Listener: ln,
		Handler:  h,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	tp := &testProxy{
		t:        t,
		addr:     ln.Addr().String(),
		handler:  h,
		worker:   w,
		cancel:   cancel,
		serveErr: make(chan error, 1),
	}
	go func() { tp.serveErr <- w.Serve(ctx) }()
	t.Cleanup(tp.Close)
	return tp
}

// ProxyAddr returns the listener address, e.g. "127.0.0.1:54321".
func (tp *testProxy) ProxyAddr() string { return tp.addr }

// Close cancels the worker context, waits up to 500ms for Serve to
// return, then cleans up pools. Used by t.Cleanup.
//
// Tests that specifically exercise the drain path call DrainWait with
// their own deadline before the cleanup runs.
func (tp *testProxy) Close() {
	tp.cancel()
	select {
	case <-tp.serveErr:
	case <-time.After(500 * time.Millisecond):
	}
	tp.handler.Pools().CloseAll()
}

// DrainWait cancels the context and waits for in-flight dispatches,
// bounded by timeout. Used by the drain test.
func (tp *testProxy) DrainWait(timeout time.Duration) bool {
	tp.cancel()
	drainCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return tp.worker.Drain(drainCtx)
}

// Reload swaps the handler's router+pools with a new pair that maps
// everything to newOrigin. Simulates a SIGHUP config reload.
func (tp *testProxy) Reload(newOrigin string) {
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{newOrigin}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	r := router.New([]router.Rule{{Host: "*", Path: "/", Upstream: "default"}})
	oldPools := tp.handler.Pools()
	tp.handler.Store(r, upstream.NewPools(defs))
	oldPools.CloseAll()
}

// ------------------------------------------------------------------
// origins
// ------------------------------------------------------------------

// startOrigin launches a net/http server on 127.0.0.1:0 with the given
// handler. Returns the addr and a cleanup.
func startOrigin(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// originAddr returns the "host:port" portion of an httptest.Server URL.
func originAddr(s *httptest.Server) string {
	// s.URL is "http://host:port"; strip the scheme.
	u := s.URL
	if len(u) > 7 && u[:7] == "http://" {
		return u[7:]
	}
	return u
}

// ------------------------------------------------------------------
// smoke test: does the basic forward path work at all?
// ------------------------------------------------------------------

func TestSmokeGETForwards(t *testing.T) {
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Origin", "hit")
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	tp := startProxy(t, originAddr(origin))

	resp, err := http.Get("http://" + tp.ProxyAddr() + "/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if h := resp.Header.Get("X-Origin"); h != "hit" {
		t.Fatalf("X-Origin: got %q want hit", h)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello /ping" {
		t.Fatalf("body: got %q want %q", body, "hello /ping")
	}
}

// ------------------------------------------------------------------
// keep-alive: one client, many requests
// ------------------------------------------------------------------

func TestKeepAlive200Requests(t *testing.T) {
	var origHit int
	var mu sync.Mutex
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		origHit++
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	tp := startProxy(t, originAddr(origin))

	tr := &http.Transport{
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
		DisableKeepAlives: false,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	for i := 0; i < 200; i++ {
		resp, err := client.Get("http://" + tp.ProxyAddr() + "/x")
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("req %d status: %d", i, resp.StatusCode)
		}
	}
	mu.Lock()
	hit := origHit
	mu.Unlock()
	if hit != 200 {
		t.Fatalf("origin hits: got %d want 200", hit)
	}
}

// ------------------------------------------------------------------
// 100-continue
// ------------------------------------------------------------------

func TestExpect100Continue(t *testing.T) {
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "got %d bytes", len(body))
	}))
	tp := startProxy(t, originAddr(origin))

	// Use a raw connection so we can inspect the 100 Continue interim
	// response directly. net/http's client doesn't expose it.
	conn, err := net.Dial("tcp", tp.ProxyAddr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	body := "hello world"
	req := fmt.Sprintf(
		"POST /upload HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n",
		len(body),
	)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write headers: %v", err)
	}

	// Expect the interim 100 within 2s.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read interim: %v", err)
	}
	got := string(buf[:n])
	if got[:len("HTTP/1.1 100")] != "HTTP/1.1 100" {
		t.Fatalf("interim response: got %q want HTTP/1.1 100 ...", got)
	}

	// Now send the body; expect the final 200.
	if _, err := conn.Write([]byte(body)); err != nil {
		t.Fatalf("write body: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	all, _ := io.ReadAll(conn)
	s := string(all)
	// We may see the remainder of the interim + the final response, or
	// only the final response depending on how the buffer split.
	if !contains(s, "HTTP/1.1 200") {
		t.Fatalf("final response missing 200: %q (plus earlier %q)", s, got)
	}
	if !contains(s, "got 11 bytes") {
		t.Fatalf("origin did not see body: %q", s)
	}
}

// TestNoExpectNo100 is the negative case: a POST without Expect does
// NOT cause the proxy to emit a 100 Continue.
func TestNoExpectNo100(t *testing.T) {
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte("ok"))
	}))
	tp := startProxy(t, originAddr(origin))

	conn, err := net.Dial("tcp", tp.ProxyAddr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	body := "payload"
	fullReq := fmt.Sprintf(
		"POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s",
		len(body), body,
	)
	if _, err := conn.Write([]byte(fullReq)); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	s := string(buf[:n])
	if contains(s, "HTTP/1.1 100") {
		t.Fatalf("unexpected 100 Continue: %q", s)
	}
	if !contains(s, "HTTP/1.1 200") {
		t.Fatalf("no 200 response: %q", s)
	}
}

// ------------------------------------------------------------------
// SIGHUP / live reload of routes+pools
// ------------------------------------------------------------------

func TestReloadSwapsUpstream(t *testing.T) {
	a := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("A"))
	}))
	b := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("B"))
	}))
	tp := startProxy(t, originAddr(a))

	client := &http.Client{Timeout: 2 * time.Second}
	get := func() string {
		resp, err := client.Get("http://" + tp.ProxyAddr() + "/")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	if got := get(); got != "A" {
		t.Fatalf("pre-reload: got %q want A", got)
	}

	tp.Reload(originAddr(b))

	// Poll until the reload is observed. With the atomic pointer swap
	// plus closed-old-pools, any request opening a fresh conn picks up
	// the new route on the very next call; clients that held open a
	// stale conn might see one more A. Use a modest bound.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() == "B" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("post-reload: never saw B within deadline")
}

// ------------------------------------------------------------------
// SIGTERM / graceful drain
// ------------------------------------------------------------------

func TestGracefulDrainCompletesInFlight(t *testing.T) {
	ready := make(chan struct{})
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(ready)
		// Simulate a slow origin. The proxy must keep the client socket
		// alive and deliver the eventual response even after the worker
		// context has been cancelled.
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte("done"))
	}))
	tp := startProxy(t, originAddr(origin))

	// Fire the slow request in a goroutine. Use a per-test transport
	// whose idle conns we close explicitly — otherwise the client holds
	// the keep-alive conn open past the request, which keeps the
	// proxy's ServeConn goroutine blocked in Read and stalls Drain.
	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	go func() {
		resp, err := client.Get("http://" + tp.ProxyAddr() + "/slow")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resCh <- result{body: string(b)}
	}()

	// Wait until the origin has started processing so we know the
	// request is actually in flight.
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatalf("origin never got the request")
	}

	// Trigger shutdown (cancel ctx, drain in-flight).
	drainOK := make(chan bool, 1)
	go func() { drainOK <- tp.DrainWait(3 * time.Second) }()

	// The in-flight request must complete with "done".
	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("in-flight request errored during drain: %v", r.err)
		}
		if r.body != "done" {
			t.Fatalf("in-flight body: got %q want done", r.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("in-flight request never completed")
	}
	if !<-drainOK {
		t.Fatalf("drain timed out despite in-flight completing")
	}
}

// ------------------------------------------------------------------
// pool failure: origin gone → fast 502
// ------------------------------------------------------------------

func TestPoolFailureReturns502Fast(t *testing.T) {
	// Listen once to grab a port, then close it so dials to this
	// address fail (connection refused). No daemon: just a dead port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	tp := startProxy(t, deadAddr)

	client := &http.Client{Timeout: 2 * time.Second}
	// First few requests should be 502s (from a dial error). They must
	// return fast — well under the client timeout.
	for i := 0; i < 5; i++ {
		start := time.Now()
		resp, err := client.Get("http://" + tp.ProxyAddr() + "/x")
		dur := time.Since(start)
		if err != nil {
			t.Fatalf("req %d: unexpected client error: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 502 {
			t.Fatalf("req %d: status %d want 502", i, resp.StatusCode)
		}
		if dur > 1500*time.Millisecond {
			t.Fatalf("req %d: took %v; expected fast failure", i, dur)
		}
	}
}

// ------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestLongKeepAliveNoEOF proves Phase 2.A's amortized deadline reset:
// a keep-alive client that fires 150 requests sees no EOF at any
// point. Before Phase 2.A this would still pass (150 requests in a
// tight loop fit inside the 2-minute window); the value of the test is
// that it won't regress if the amortization math is wrong, AND — with
// the 64-request-cap branch — it exercises the bump firing once during
// the run without relying on wall-clock advancement.
func TestLongKeepAliveNoEOF(t *testing.T) {
	var hits int
	var mu sync.Mutex
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte("ok"))
	}))
	tp := startProxy(t, originAddr(origin))

	tr := &http.Transport{
		MaxIdleConns:    1,
		IdleConnTimeout: 5 * time.Minute,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	// 150 > DeadlineMaxUses (64), so at least two bumps happen on the
	// client deadline during the run. All must succeed.
	for i := 0; i < 150; i++ {
		resp, err := client.Get("http://" + tp.ProxyAddr() + "/x")
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("req %d: status %d", i, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 150 {
		t.Fatalf("origin hits: got %d want 150", hits)
	}
}

// TestChunkedUploadForwards verifies POST with Transfer-Encoding:
// chunked flows through the handler to the origin.
//
// The current Phase 1 stdlib path uses spliceAll for chunked bodies:
// it streams until the client half-closes, trusting the upstream to
// parse. Phase 2.C replaces this with a validating ChunkedReader that
// terminates on 0\r\n\r\n. We simulate the Phase 2.C-ready client
// behavior here by CloseWriting after the terminator.
func TestChunkedUploadForwards(t *testing.T) {
	var got []byte
	var mu sync.Mutex
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got[:0], b...)
		mu.Unlock()
		fmt.Fprintf(w, "%d", len(b))
	}))
	tp := startProxy(t, originAddr(origin))

	// Build a valid chunked body: "4\r\nwiki\r\n5\r\npedia\r\n0\r\n\r\n".
	body := "4\r\nwiki\r\n5\r\npedia\r\n0\r\n\r\n"
	req := "POST /upload HTTP/1.1\r\n" +
		"Host: x\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Connection: close\r\n\r\n" +
		body

	conn, err := net.Dial("tcp", tp.ProxyAddr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Half-close the write side so the current spliceAll path sees EOF
	// at the terminator boundary. Phase 2.C will not require this.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	all, _ := io.ReadAll(conn)
	s := string(all)
	if !contains(s, "HTTP/1.1 200") {
		t.Fatalf("status missing 200: %q", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if string(got) != "wikipedia" {
		t.Fatalf("origin body: got %q want wikipedia", got)
	}
}

// TestMain tightens the default test timeout log to stderr so CI
// failures surface cleanly.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
