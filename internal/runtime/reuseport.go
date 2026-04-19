package runtime

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// Listen binds addr with SO_REUSEPORT set so multiple worker processes can
// share the same listening port and have the kernel spread incoming
// connections across them.
//
// On non-Linux systems SO_REUSEPORT semantics differ (BSD permits multiple
// listeners but without kernel load-balancing); we still set it so dev on
// macOS succeeds. The bench numbers that matter come from Linux.
func Listen(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			if err := c.Control(func(fd uintptr) {
				// Allow multiple workers to bind the same port.
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
				if opErr != nil {
					return
				}
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			return opErr
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}
