// Raw listen socket setup.
//
// We bypass net.Listen for the uring worker because Go's netpoll will
// otherwise register the fd with epoll, and competing with the kernel's
// io_uring accept path causes wrong-fd / zero-state weirdness. One
// owner, one dispatch — io_uring is the sole consumer of this fd.

//go:build linux

package uring

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// ListenRaw binds addr with SO_REUSEPORT and returns the raw fd in
// listening state. addr is "host:port"; host empty means INADDR_ANY.
func ListenRaw(addr string) (int, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return -1, err
	}
	var ip4 [4]byte
	if host != "" && host != "0.0.0.0" {
		ip := net.ParseIP(host).To4()
		if ip == nil {
			return -1, fmt.Errorf("uring: listen: not IPv4: %s", host)
		}
		copy(ip4[:], ip)
	}
	var port uint16
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return -1, fmt.Errorf("uring: listen: bad port %q", portStr)
		}
		port = port*10 + uint16(c-'0')
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		return -1, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		unix.Close(fd)
		return -1, err
	}
	sa := &unix.SockaddrInet4{Port: int(port), Addr: ip4}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("uring: bind %s: %w", addr, err)
	}
	if err := unix.Listen(fd, 65535); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}
