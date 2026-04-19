//go:build integration

package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cur "tachyon/internal/intent/generated/current"
	"tachyon/internal/proxy"
	"tachyon/internal/router"
	trt "tachyon/internal/runtime"
	"tachyon/internal/upstream"
)

func startProxyWithRoutes(t *testing.T, routes []router.Rule, defs map[string]router.Upstream) *testProxy {
	t.Helper()
	ln, err := trt.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	intents, err := cur.BuildRoutePrograms(routes)
	if err != nil {
		t.Fatalf("bind intents: %v", err)
	}
	h := proxy.NewHandler(router.New(routes), upstream.NewPools(defs), intents)
	w := &trt.Worker{Listener: ln, Handler: h}
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

func TestIntentHeadersAndTerminalResponse(t *testing.T) {
	var sawProxyHeader string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProxyHeader = r.Header.Get("X-Proxy")
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	t.Cleanup(origin.Close)

	routes := []router.Rule{{
		RouteID: 0,
		Host:    "*",
		Path:    "/",
		Upstream: "default",
		Intents: []string{"sample_headers", "sample_terminal"},
	}}
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr(origin)}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	tp := startProxyWithRoutes(t, routes, defs)

	req, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/", nil)
	req.Host = "example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Served-By"); got != "tachyon" {
		t.Fatalf("response header: got %q want tachyon", got)
	}
	if sawProxyHeader != "tachyon" {
		t.Fatalf("origin x-proxy: got %q want tachyon", sawProxyHeader)
	}

	req2, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/blocked", nil)
	req2.Host = "example.com"
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("do blocked: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != 451 {
		t.Fatalf("blocked status: got %d want 451", resp2.StatusCode)
	}
	if string(body) != "blocked by intent" {
		t.Fatalf("blocked body: got %q", body)
	}
}

func TestIntentQueryFilter(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(origin.Close)

	routes := []router.Rule{{
		RouteID:  0,
		Host:     "*",
		Path:     "/",
		Upstream: "default",
		Intents:  []string{"sample_query_filter"},
	}}
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr(origin)}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	tp := startProxyWithRoutes(t, routes, defs)

	// debug=1 → 403
	req, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/search?debug=1", nil)
	req.Host = "example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("debug=1 status: got %d want 403", resp.StatusCode)
	}

	// debug=0 → 200
	req2, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/search?debug=0", nil)
	req2.Host = "example.com"
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("debug=0 status: got %d want 200", resp2.StatusCode)
	}
}

func TestIntentCookieMatch(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(origin.Close)

	routes := []router.Rule{{
		RouteID:  0,
		Host:     "*",
		Path:     "/",
		Upstream: "default",
		Intents:  []string{"sample_cookie_auth"},
	}}
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr(origin)}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	tp := startProxyWithRoutes(t, routes, defs)
	client := &http.Client{}

	// role=admin cookie → upstream receives x-role header
	var sawRoleHeader string
	originCapture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRoleHeader = r.Header.Get("X-Role")
		w.WriteHeader(200)
	}))
	t.Cleanup(originCapture.Close)
	routes2 := []router.Rule{{
		RouteID:  0,
		Host:     "*",
		Path:     "/",
		Upstream: "default",
		Intents:  []string{"sample_cookie_auth"},
	}}
	defs2 := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr(originCapture)}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	tp2 := startProxyWithRoutes(t, routes2, defs2)

	req, _ := http.NewRequest(http.MethodGet, "http://"+tp2.ProxyAddr()+"/admin/page", nil)
	req.Host = "example.com"
	req.AddCookie(&http.Cookie{Name: "role", Value: "admin"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("admin request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if sawRoleHeader != "admin" {
		t.Fatalf("x-role header at origin: got %q want admin", sawRoleHeader)
	}

	// No cookie → policy doesn't match, x-role not set
	sawRoleHeader = ""
	req2, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/admin/page", nil)
	req2.Host = "example.com"
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("no-cookie request: %v", err)
	}
	resp2.Body.Close()
	_ = resp2
}

func TestIntentRateLimit(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(origin.Close)

	routes := []router.Rule{{
		RouteID: 0,
		Host:    "*",
		Path:    "/",
		Upstream: "default",
		Intents: []string{"sample_rate_limit"},
	}}
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr(origin)}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	tp := startProxyWithRoutes(t, routes, defs)

	client := &http.Client{}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/limited", nil)
		req.Header.Set("X-Api-Key", "gold")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("req %d: %v", i, err)
		}
		resp.Body.Close()
		if i == 0 && resp.StatusCode != 200 {
			t.Fatalf("first status: got %d want 200", resp.StatusCode)
		}
		if i == 1 && resp.StatusCode != 429 {
			t.Fatalf("second status: got %d want 429", resp.StatusCode)
		}
	}
}
