package loadbalance

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestParseStatusLine covers the status-line parser on both happy and
// error paths, including edge cases that would previously panic (short
// buffers, non-HTTP input).
func TestParseStatusLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"200 OK", "HTTP/1.1 200 OK\r\n", 200},
		{"301", "HTTP/1.1 301 Moved Permanently\r\n", 301},
		{"404", "HTTP/1.1 404 Not Found\r\n", 404},
		{"500", "HTTP/1.1 500 Internal\r\n", 500},
		{"503", "HTTP/1.1 503 Service Unavailable\r\n", 503},
		{"HTTP/1.0", "HTTP/1.0 200 OK\r\n", 200},
		{"too short", "HTTP/1.", 999},
		{"empty", "", 999},
		{"not HTTP", "GARBAGE", 999},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseStatusLine([]byte(c.in))
			if got != c.want {
				t.Fatalf("parseStatusLine(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// TestProberHealthy spins up a real TCP listener serving 200 OK and
// confirms the prober marks the address as healthy after the first
// probe round.
func TestProberHealthy(t *testing.T) {
	addr := serveHTTP(t, http.StatusOK)
	pr := NewProber([]string{addr}, ProbeConfig{
		Interval: time.Hour, // we'll trigger manually
		Timeout:  time.Second,
		Path:     "/health",
	})
	// Probe immediately (loop hasn't started; call directly).
	pr.probeOne(0, addr)
	if !pr.Healthy(0) {
		t.Fatal("expected healthy after 200 OK")
	}
}

// TestProberUnhealthyOn500 confirms a 500 response marks the address
// unhealthy.
func TestProberUnhealthyOn500(t *testing.T) {
	addr := serveHTTP(t, http.StatusInternalServerError)
	pr := NewProber([]string{addr}, ProbeConfig{
		Interval: time.Hour,
		Timeout:  time.Second,
		Path:     "/health",
	})
	pr.probeOne(0, addr)
	if pr.Healthy(0) {
		t.Fatal("expected unhealthy after 500")
	}
}

// TestProberUnhealthyOnDialFailure confirms that a TCP dial failure
// (no server) marks the address unhealthy.
func TestProberUnhealthyOnDialFailure(t *testing.T) {
	pr := NewProber([]string{"127.0.0.1:1"}, ProbeConfig{
		Interval: time.Hour,
		Timeout:  50 * time.Millisecond,
		Path:     "/health",
	})
	pr.probeOne(0, "127.0.0.1:1")
	if pr.Healthy(0) {
		t.Fatal("expected unhealthy after dial failure")
	}
}

// TestProberRecovery simulates an address that starts unhealthy and
// later recovers: marks it unhealthy manually, then probes against a
// live 200 server and confirms it becomes healthy again.
func TestProberRecovery(t *testing.T) {
	addr := serveHTTP(t, http.StatusOK)
	pr := NewProber([]string{addr}, ProbeConfig{
		Interval: time.Hour,
		Timeout:  time.Second,
		Path:     "/health",
	})
	// Force unhealthy.
	pr.healthy[0].Store(false)
	// Probe against the live server.
	pr.probeOne(0, addr)
	if !pr.Healthy(0) {
		t.Fatal("expected recovery to healthy after 200")
	}
}

// TestProberStartStop confirms the background goroutine starts and
// stops cleanly with no race or deadlock, even when it hasn't yet
// completed a probe interval.
func TestProberStartStop(t *testing.T) {
	addr := serveHTTP(t, http.StatusOK)
	pr := NewProber([]string{addr}, ProbeConfig{
		Interval: time.Second,
		Timeout:  200 * time.Millisecond,
		Path:     "/health",
	})
	pr.Start()
	// Let the first probe round run.
	time.Sleep(50 * time.Millisecond)
	pr.Stop()
	if !pr.Healthy(0) {
		t.Fatal("should be healthy after successful probe")
	}
}

// TestProberInitiallyHealthy confirms that a freshly constructed Prober
// reports all addresses as healthy before any probe has run.
func TestProberInitiallyHealthy(t *testing.T) {
	pr := NewProber([]string{"127.0.0.1:1", "127.0.0.1:2"}, ProbeConfig{
		Interval: time.Hour,
		Timeout:  time.Second,
	})
	for i := range pr.addrs {
		if !pr.Healthy(i) {
			t.Fatalf("addr[%d] should start healthy", i)
		}
	}
}

// serveHTTP starts an HTTP server that responds with status on all
// requests. It registers a cleanup to shut it down and returns the
// listener address.
func serveHTTP(t *testing.T, status int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintf(w, "%d", status)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(l) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return l.Addr().String()
}
