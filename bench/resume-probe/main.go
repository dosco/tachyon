// Command resume-probe measures how often TLS session-ticket
// resumption succeeds against a listener that has N SO_REUSEPORT
// workers behind it. Each probe opens a fresh TCP connection (so
// the kernel dispatches to whichever worker is least loaded), runs
// the handshake with a client-side session cache, and checks
// ConnectionState().DidResume on the second-and-onward connections.
//
// Use it to validate Phase A of the shared-ticket-key work: before
// the fix, DidResume on a 4-worker box is ~25 %; after the fix,
// ~100 %.
//
// Usage:
//
//	resume-probe -addr 127.0.0.1:8443 -n 200
//	resume-probe -addr 127.0.0.1:8443 -n 200 -alpn h2
//
// Exit code: 0 regardless; the stdout JSON carries the numbers.
// Consumers parse "resume_rate": 0.98 etc.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8443", "server address")
	n := flag.Int("n", 200, "number of connections after the priming one")
	alpn := flag.String("alpn", "", "ALPN string (empty = no ALPN negotiation)")
	flag.Parse()

	cache := tls.NewLRUClientSessionCache(16)
	cfg := &tls.Config{
		InsecureSkipVerify: true,
		ClientSessionCache: cache,
		MinVersion:         tls.VersionTLS13,
	}
	if *alpn != "" {
		cfg.NextProtos = []string{*alpn}
	}

	// First connection primes the cache. The session ticket arrives
	// as a post-handshake message; crypto/tls surfaces it into the
	// ClientSessionCache before Close returns, so one completed
	// handshake is enough.
	if err := oneConn(*addr, cfg); err != nil {
		die("prime handshake failed: %v", err)
	}

	resumed := 0
	start := time.Now()
	for i := 0; i < *n; i++ {
		did, err := oneConnResume(*addr, cfg)
		if err != nil {
			// One failure doesn't kill the run — record it but keep
			// going, so a flaky worker doesn't hide the majority.
			fmt.Fprintf(os.Stderr, "probe %d: %v\n", i, err)
			continue
		}
		if did {
			resumed++
		}
	}
	elapsed := time.Since(start)

	rate := float64(resumed) / float64(*n)
	out := map[string]any{
		"addr":         *addr,
		"alpn":         *alpn,
		"total":        *n,
		"resumed":      resumed,
		"resume_rate":  rate,
		"elapsed_ms":   elapsed.Milliseconds(),
		"per_conn_ms":  float64(elapsed.Microseconds()) / float64(*n) / 1000,
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// oneConn opens a connection and closes it, letting any session
// ticket the server sends land in cfg.ClientSessionCache.
func oneConn(addr string, cfg *tls.Config) error {
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return err
	}
	// Force a post-handshake read so NewSessionTicket is processed.
	// crypto/tls delivers tickets on the read side; writing alone
	// doesn't flush the incoming queue on all stdlib versions.
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var buf [1]byte
	_, _ = c.Read(buf[:])
	return c.Close()
}

// oneConnResume opens a connection and reports whether the handshake
// resumed. Any session ticket the server issues lands in the cache
// for the next iteration.
func oneConnResume(addr string, cfg *tls.Config) (bool, error) {
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return false, err
	}
	defer c.Close()
	st := c.ConnectionState()
	// Drain any post-handshake ticket for the next iteration.
	_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	var buf [1]byte
	_, _ = c.Read(buf[:])
	return st.DidResume, nil
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
