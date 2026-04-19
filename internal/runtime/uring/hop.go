// Hop-by-hop header filtering, carbon-copied from internal/proxy. Kept
// local so this package has no dependency on internal/proxy (which owns
// the net.Conn-based handler we're replacing).

//go:build linux

package uring

import "tachyon/http1"

var hopByHop = [][]byte{
	[]byte("Connection"),
	[]byte("Keep-Alive"),
	[]byte("Proxy-Authenticate"),
	[]byte("Proxy-Authorization"),
	[]byte("TE"),
	[]byte("Trailer"),
	[]byte("Transfer-Encoding"),
	[]byte("Upgrade"),
}

func isHopByHop(name []byte) bool {
	for _, h := range hopByHop {
		if http1.EqualFold(name, h) {
			return true
		}
	}
	return false
}
