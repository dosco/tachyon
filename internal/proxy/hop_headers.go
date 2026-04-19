package proxy

import "tachyon/http1"

// Hop-by-hop headers listed by RFC 7230 §6.1. These must be stripped when
// forwarding a request or response, because they refer to the single hop
// between the previous and next party, not to the end-to-end message.
//
// We keep the list as a package-level []([]byte) so EqualFold can compare
// against it without allocating.
var hopHeaders = [...][]byte{
	http1.HdrConnection,
	http1.HdrKeepAlive,
	http1.HdrProxyConnection,
	http1.HdrTE,
	http1.HdrTrailer,
	http1.HdrUpgrade,
	[]byte("proxy-authenticate"),
	[]byte("proxy-authorization"),
	[]byte("transfer-encoding"),
}

// isHopByHop reports whether name is a hop-by-hop header. name is compared
// case-insensitively against the lowercase constants.
func isHopByHop(name []byte) bool {
	for _, h := range hopHeaders {
		if http1.EqualFold(name, h) {
			return true
		}
	}
	return false
}
