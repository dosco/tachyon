//go:build integration

package e2e

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"tachyon/metrics"
)

// TestMetricsCountersRecordOutcomes sends a mix of 2xx, 4xx, and 5xx
// responses through the proxy and asserts the global counters moved by
// the right amounts. It uses direct reads of metrics.Global rather than
// spinning up the debug server — the server wiring is tested separately
// in the main package.
func TestMetricsCountersRecordOutcomes(t *testing.T) {
	// Snapshot baseline — other tests in this suite also exercise the
	// handler and have bumped counters. We measure deltas.
	base := metrics.Read()

	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(200)
		case "/bad":
			w.WriteHeader(400)
		case "/down":
			w.WriteHeader(503)
		default:
			w.WriteHeader(404)
		}
	}))
	tp := startProxy(t, originAddr(origin))

	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   3 * time.Second,
	}
	paths := []struct {
		path string
		want int
	}{
		{"/ok", 200}, {"/ok", 200},
		{"/bad", 400},
		{"/down", 503},
	}
	for _, tc := range paths {
		resp, err := client.Get("http://" + tp.ProxyAddr() + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: got %d want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	got := metrics.Read()
	if d := got.Requests - base.Requests; d != 4 {
		t.Errorf("Requests delta: got %d want 4", d)
	}
	if d := got.OK2xx - base.OK2xx; d != 2 {
		t.Errorf("OK2xx delta: got %d want 2", d)
	}
	if d := got.Err4xx - base.Err4xx; d != 1 {
		t.Errorf("Err4xx delta: got %d want 1", d)
	}
	if d := got.Err5xx - base.Err5xx; d != 1 {
		t.Errorf("Err5xx delta: got %d want 1", d)
	}
}

// TestMetricsPrometheusScrape verifies the text-exposition output is
// well-formed and contains all metric families.
func TestMetricsPrometheusScrape(t *testing.T) {
	// Emit some traffic so non-zero counts are present.
	origin := startOrigin(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	tp := startProxy(t, originAddr(origin))

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 3; i++ {
		resp, err := client.Get("http://" + tp.ProxyAddr() + "/")
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}

	var out strings.Builder
	if err := metrics.WritePrometheus(&out); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	s := out.String()
	// Families present.
	for _, name := range []string{
		"tachyon_requests_total",
		"tachyon_responses_total",
		"tachyon_upstream_errors_total",
	} {
		if !strings.Contains(s, "# TYPE "+name) {
			t.Errorf("missing TYPE line for %s", name)
		}
	}
	// Basic syntax: every non-# line looks like `name value` or `name{labels} value`.
	for _, line := range strings.Split(s, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		// Ends with a decimal integer.
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Errorf("malformed metric line: %q", line)
			continue
		}
		if _, err := fmt.Sscanf(fields[1], "%d", new(uint64)); err != nil {
			t.Errorf("non-integer value: %q", line)
		}
	}
}

// TestMetricsUpDialErrorCounted stops the origin, then makes a request.
// The pool's Acquire fails — we expect a 502 to the client and
// UpDialErr to bump.
func TestMetricsUpDialErrorCounted(t *testing.T) {
	// Grab a free port, close it, then use it as the origin addr so
	// any dial attempt fails immediately with "connection refused".
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := l.Addr().String()
	_ = l.Close()

	tp := startProxy(t, deadAddr)

	base := metrics.Read()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + tp.ProxyAddr() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatalf("status: got %d want 502", resp.StatusCode)
	}
	got := metrics.Read()
	if d := got.UpDialErr - base.UpDialErr; d < 1 {
		t.Errorf("UpDialErr delta: got %d want >=1", d)
	}
	if d := got.Err5xx - base.Err5xx; d != 1 {
		t.Errorf("Err5xx delta: got %d want 1", d)
	}
}
