//go:build integration

package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tachyon/internal/router"
)

func TestExampleWorkflowPolicies(t *testing.T) {
	var sawExampleHeader string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawExampleHeader = r.Header.Get("X-Example-Proxy")
		w.Header().Set("X-Origin-Seen-Proxy", sawExampleHeader)
		w.WriteHeader(200)
		w.Write([]byte("origin ok"))
	}))
	t.Cleanup(origin.Close)

	routes := []router.Rule{{
		RouteID:  0,
		Host:     "example.local",
		Path:     "/",
		Upstream: "default",
		Intents:  []string{"example_block_admin_debug", "example_proxy_headers"},
	}}
	defs := map[string]router.Upstream{
		"default": {Addrs: []string{originAddr(origin)}, IdlePerHost: 8, ConnectTimeout: time.Second},
	}
	tp := startProxyWithRoutes(t, routes, defs)
	client := &http.Client{}

	req, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/hello", nil)
	req.Host = "example.local"
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("forward request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("forward status: got %d want 200", resp.StatusCode)
	}
	if sawExampleHeader != "tachyon" {
		t.Fatalf("origin saw x-example-proxy: got %q want tachyon", sawExampleHeader)
	}
	if got := resp.Header.Get("X-Example-Served-By"); got != "tachyon" {
		t.Fatalf("response x-example-served-by: got %q want tachyon", got)
	}
	if got := resp.Header.Get("X-Origin-Seen-Proxy"); got != "tachyon" {
		t.Fatalf("response x-origin-seen-proxy: got %q want tachyon", got)
	}
	if string(body) != "origin ok" {
		t.Fatalf("forward body: got %q want %q", body, "origin ok")
	}

	sawExampleHeader = ""
	req2, _ := http.NewRequest(http.MethodGet, "http://"+tp.ProxyAddr()+"/admin?debug=1", nil)
	req2.Host = "example.local"
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("blocked request: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Fatalf("blocked status: got %d want 403", resp2.StatusCode)
	}
	if sawExampleHeader != "" {
		t.Fatalf("blocked request should not reach origin, saw %q", sawExampleHeader)
	}
}
