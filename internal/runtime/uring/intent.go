//go:build linux

package uring

import (
	"net/netip"
	"strings"

	"golang.org/x/sys/unix"
	"tachyon/http1"
	irt "tachyon/internal/intent/runtime"
)

type intentRequestView struct {
	req      *http1.Request
	host     string
	path     string
	clientIP string
}

func (v intentRequestView) Method() string   { return string(v.req.MethodBytes()) }
func (v intentRequestView) Path() string     { return v.path }
func (v intentRequestView) Host() string     { return v.host }
func (v intentRequestView) ClientIP() string { return v.clientIP }
func (v intentRequestView) Header(name string) string {
	return string(v.req.Lookup([]byte(name)))
}
func (v intentRequestView) Query(name string) string {
	q := strings.IndexByte(v.path, '?')
	if q < 0 {
		return ""
	}
	return rawQueryLookup(v.path[q+1:], name)
}
func (v intentRequestView) Cookie(name string) string {
	return rawCookieLookup(string(v.req.Lookup([]byte("cookie"))), name)
}

func rawQueryLookup(query, name string) string {
	for query != "" {
		var pair string
		pair, query, _ = strings.Cut(query, "&")
		k, v, _ := strings.Cut(pair, "=")
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func rawCookieLookup(header, name string) string {
	for header != "" {
		var pair string
		pair, header, _ = strings.Cut(header, ";")
		pair = strings.TrimSpace(pair)
		k, v, _ := strings.Cut(pair, "=")
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func intentHeaderRemoved(muts irt.HeaderMutations, name []byte) bool {
	return muts.Removed(string(name))
}

func intentHeaderOverridden(muts irt.HeaderMutations, name []byte) bool {
	return muts.Overridden(string(name))
}

func peerIP(fd int) string {
	sa, err := unix.Getpeername(fd)
	if err != nil {
		return ""
	}
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrFrom4(v.Addr).String()
	case *unix.SockaddrInet6:
		return netip.AddrFrom16(v.Addr).String()
	default:
		return ""
	}
}
