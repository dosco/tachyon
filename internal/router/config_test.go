package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes body to a temp file and returns its path. The
// caller gets a path it can feed directly to Load.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestLoadRejectsBothUpstreamForms covers the shorthand-vs-weighted
// exclusivity rule: a route may set `upstream` or `upstreams` but not
// both, since the meaning is ambiguous.
func TestLoadRejectsBothUpstreamForms(t *testing.T) {
	p := writeConfig(t, `
listen: ":0"
routes:
  - host: "h"
    path: "/"
    upstream: "a"
    upstreams:
      - name: "a"
        weight: 1
upstreams:
  a:
    addrs: ["127.0.0.1:9000"]
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("expected 'both' error, got %v", err)
	}
}

// TestLoadRejectsNoUpstream confirms a route with neither form is a
// config error rather than silently matching nothing.
func TestLoadRejectsNoUpstream(t *testing.T) {
	p := writeConfig(t, `
listen: ":0"
routes:
  - host: "h"
    path: "/"
upstreams:
  a:
    addrs: ["127.0.0.1:9000"]
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "no upstream") {
		t.Fatalf("expected 'no upstream' error, got %v", err)
	}
}

// TestLoadAcceptsWeightedUpstreams confirms a valid weighted route
// parses cleanly and zero weights are normalised to 1.
func TestLoadAcceptsWeightedUpstreams(t *testing.T) {
	p := writeConfig(t, `
listen: ":0"
routes:
  - host: "h"
    path: "/"
    upstreams:
      - name: "a"
        weight: 0
      - name: "b"
        weight: 2
upstreams:
  a:
    addrs: ["127.0.0.1:9000"]
  b:
    addrs: ["127.0.0.1:9001"]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Routes) != 1 {
		t.Fatalf("got %d routes", len(c.Routes))
	}
	wu := c.Routes[0].Upstreams
	if len(wu) != 2 {
		t.Fatalf("got %d upstream entries", len(wu))
	}
	if wu[0].Weight != 1 {
		t.Fatalf("zero weight not normalised: %d", wu[0].Weight)
	}
	if wu[1].Weight != 2 {
		t.Fatalf("weight preserved: got %d want 2", wu[1].Weight)
	}
	if c.Routes[0].RouteID != 0 {
		t.Fatalf("route id: got %d want 0", c.Routes[0].RouteID)
	}
}

func TestLoadPreservesIntentsAndRouteID(t *testing.T) {
	p := writeConfig(t, `
listen: ":0"
routes:
  - host: "h"
    path: "/"
    upstream: "a"
    intents: ["api_guard", "tenant_headers"]
upstreams:
  a:
    addrs: ["127.0.0.1:9000"]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(c.Routes[0].Intents); got != 2 {
		t.Fatalf("intents: got %d want 2", got)
	}
	if c.Routes[0].Intents[0] != "api_guard" || c.Routes[0].Intents[1] != "tenant_headers" {
		t.Fatalf("intents: got %v", c.Routes[0].Intents)
	}
	if c.Routes[0].RouteID != 0 {
		t.Fatalf("route id: got %d want 0", c.Routes[0].RouteID)
	}
}
