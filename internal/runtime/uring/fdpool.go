// Per-worker upstream fd pool.
//
// Pingora's trick is two-tier (hot + global); we rely on GOMAXPROCS=1
// per worker so the "hot" tier is a plain slice, no atomics. On startup
// we pre-dial `idlePerHost` fds per upstream so the event loop never
// blocks on connect(2) for a warm upstream.
//
// If the pool runs empty (huge burst), we synchronously dial a fresh
// fd. That's blocking, but rare — fds only return to the pool after a
// full request/response cycle, so the ceiling of in-flight upstreams
// equals the ceiling of concurrent client requests, which matches the
// pre-dial count in the production-tuned path.

//go:build linux

package uring

import (
	"fmt"
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// fdPool is a stack of ready-to-write upstream fds for a single addr.
type fdPool struct {
	addr   string
	host   [4]byte // IPv4 only for now; the bench origin is 127.0.0.1
	port   uint16
	stack  []int
	max    int
}

func newFDPool(addr string, max int) (*fdPool, error) {
	host, port, err := parseIPv4(addr)
	if err != nil {
		return nil, err
	}
	p := &fdPool{addr: addr, host: host, port: port, stack: make([]int, 0, max), max: max}
	// Pre-dial. Any failure here is fatal because the proxy can't serve
	// without an upstream — a pingora/nginx misconfig would behave the
	// same.
	for i := 0; i < max; i++ {
		fd, err := p.dial()
		if err != nil {
			return nil, fmt.Errorf("fdpool: pre-dial %s: %w", addr, err)
		}
		p.stack = append(p.stack, fd)
	}
	return p, nil
}

// acquire pops an fd. Returns -1 if empty; caller can then dial().
func (p *fdPool) acquire() int {
	n := len(p.stack)
	if n == 0 {
		return -1
	}
	fd := p.stack[n-1]
	p.stack = p.stack[:n-1]
	return fd
}

// release pushes an fd back. If pool is full, closes the fd.
func (p *fdPool) release(fd int) {
	if len(p.stack) >= p.max {
		unix.Close(fd)
		return
	}
	p.stack = append(p.stack, fd)
}

// dial opens a fresh TCP conn to the pool's addr. Blocking — callers
// on the hot path should acquire() first and only dial on miss.
func (p *fdPool) dial() (int, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		return -1, err
	}
	// TCP_NODELAY: a proxy's request is tiny and must flush immediately.
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	sa := &unix.SockaddrInet4{Port: int(p.port), Addr: p.host}
	// Use a short timeout via a tempnonblock + select... actually the
	// origin is local and dials always succeed in <1ms, so blocking
	// connect is fine at startup. If this ever fails we fail loudly.
	if err := unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// addr helpers ---------------------------------------------------------

func parseIPv4(s string) ([4]byte, uint16, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return [4]byte{}, 0, err
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return [4]byte{}, 0, fmt.Errorf("fdpool: not IPv4: %s", host)
	}
	var p uint16
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return [4]byte{}, 0, fmt.Errorf("fdpool: bad port: %s", portStr)
		}
		p = p*10 + uint16(c-'0')
	}
	_ = time.Now // keep time imported if we later add dial deadlines
	var out [4]byte
	copy(out[:], ip)
	return out, p, nil
}

// parseIPv4SupportsError is a tiny helper the pool init path uses.
var _ = syscall.EINVAL
