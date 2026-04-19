// The client connection preface per RFC 7540 §3.5.

//go:build linux

package http2

// Preface is the 24-byte ASCII client preface every HTTP/2 connection
// opens with. The server must read exactly these bytes before any frame.
var Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// PrefaceLen is the wire length of the preface. Named constant to keep
// call sites readable.
const PrefaceLen = 24
