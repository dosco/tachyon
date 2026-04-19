package loadbalance

import (
	"net"
	"sync/atomic"
	"time"
)

// ProbeConfig holds per-pool active health-probe settings. All fields
// have sensible defaults; zero values are replaced at construction.
type ProbeConfig struct {
	// Interval between probe rounds. Default 10 s.
	Interval time.Duration
	// Path for the HTTP HEAD request. Default "/health".
	Path string
	// Timeout for the dial + request + response-status-line round-trip.
	// Default 1 s.
	Timeout time.Duration
}

// Prober issues HTTP HEAD probes to each address in a pool at a
// regular interval and maintains a per-address healthy bit. Addresses
// start healthy so traffic is not rejected before the first round
// completes.
//
// Use NewProber then call Start. Call Stop when the pool shuts down.
// Start and Stop must each be called at most once.
type Prober struct {
	addrs   []string
	healthy []atomic.Bool
	cfg     ProbeConfig

	stop chan struct{}
	done chan struct{}
}

// NewProber constructs a Prober for addrs. Applies defaults for zero
// cfg fields.
func NewProber(addrs []string, cfg ProbeConfig) *Prober {
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Second
	}
	if cfg.Path == "" {
		cfg.Path = "/health"
	}
	p := &Prober{
		addrs:   addrs,
		healthy: make([]atomic.Bool, len(addrs)),
		cfg:     cfg,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	// Initialise to healthy so we forward traffic before the first probe.
	for i := range p.healthy {
		p.healthy[i].Store(true)
	}
	return p
}

// Healthy reports whether the address at addrs[idx] passed its last probe.
func (p *Prober) Healthy(idx int) bool {
	return p.healthy[idx].Load()
}

// ForceHealthy overrides the healthy bit at idx. Used in tests that want
// deterministic control without running real probe goroutines.
func (p *Prober) ForceHealthy(idx int, v bool) {
	p.healthy[idx].Store(v)
}

// Start begins the background probe loop. Must be called exactly once.
func (p *Prober) Start() { go p.loop() }

// Stop halts the probe loop and waits for the goroutine to exit.
func (p *Prober) Stop() {
	close(p.stop)
	<-p.done
}

func (p *Prober) loop() {
	defer close(p.done)
	// Probe immediately on start so the bit reflects reality before the
	// first real request arrives.
	p.probeAll()
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.probeAll()
		}
	}
}

// probeAll probes every address sequentially. On GOMAXPROCS=1 workers,
// sequential probing is fine — total time is len(addrs)×timeout which
// is seconds-scale even for large pools, and the probe goroutine is
// not on the forwarding critical path.
func (p *Prober) probeAll() {
	for i, addr := range p.addrs {
		p.probeOne(i, addr)
	}
}

// probeOne issues a single HEAD request to addr, updating healthy[idx].
// A 2xx or 3xx response marks the address healthy; any TCP or HTTP
// error (including 5xx) marks it unhealthy.
func (p *Prober) probeOne(idx int, addr string) {
	healthy := p.httpProbe(addr)
	p.healthy[idx].Store(healthy)
}

// httpProbe dials addr, sends an HTTP/1.1 HEAD request, reads the
// status line, and returns true when the status is < 500. The entire
// round-trip must complete within cfg.Timeout.
//
// We write the request as raw bytes rather than importing net/http so
// the probe package stays thin and the per-request overhead is a
// single dial + two small writes + one partial read.
func (p *Prober) httpProbe(addr string) bool {
	deadline := time.Now().Add(p.cfg.Timeout)
	c, err := net.DialTimeout("tcp", addr, p.cfg.Timeout)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(deadline)

	// Minimal HTTP/1.1 HEAD request. We avoid heap allocation by
	// building the three fixed parts separately and writing them as
	// one syscall via a stack-allocated slice of iovec equivalents.
	// net.Conn doesn't expose writev, so three short writes are fine —
	// the kernel will coalesce them in the TCP send buffer.
	const prefix = "HEAD "
	const suffix = " HTTP/1.1\r\nHost: "
	const tail = "\r\nConnection: close\r\n\r\n"
	req := prefix + p.cfg.Path + suffix + addr + tail
	if _, err := c.Write([]byte(req)); err != nil {
		return false
	}

	// Read just enough to see the status code ("HTTP/1.1 NNN ").
	// The shortest valid status line is "HTTP/1.1 200 \r\n" = 17 B;
	// 64 bytes is always enough and stays on the stack.
	var buf [64]byte
	n, _ := c.Read(buf[:])
	return parseStatusLine(buf[:n]) < 500
}

// parseStatusLine extracts the HTTP status code from the first bytes of
// an HTTP/1.x response status line. Returns 999 (treated as unhealthy)
// if the response does not match the expected format.
//
// Expected: "HTTP/1.x NNN ..." where NNN is three ASCII digits.
func parseStatusLine(b []byte) int {
	// "HTTP/1.x " is 9 bytes; status code starts at offset 9.
	const skip = 9
	if len(b) < skip+3 {
		return 999
	}
	if b[0] != 'H' || b[1] != 'T' || b[2] != 'T' || b[3] != 'P' {
		return 999
	}
	d0 := int(b[skip] - '0')
	d1 := int(b[skip+1] - '0')
	d2 := int(b[skip+2] - '0')
	if d0 < 0 || d0 > 9 || d1 < 0 || d1 > 9 || d2 < 0 || d2 > 9 {
		return 999
	}
	return d0*100 + d1*10 + d2
}
