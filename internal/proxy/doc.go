// Package proxy is the glue that makes tachyon a proxy: it ties together
// http1 (parsing), router (routing), and upstream (pool) into the per-
// connection loop that serves one client forever (until close).
//
// It is deliberately short. Everything heavy - parsing, pooling, io -
// happens in the libraries. The proxy is the seam.
package proxy
