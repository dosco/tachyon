// Package qpack implements a QPACK (RFC 9204) encoder and decoder
// suitable for an HTTP/3 server.
//
// Scope:
//
//   - Encoder: static-table references only. `Encode` emits indexed
//     fields for exact static-table matches, literal-with-static-name-
//     reference for name matches, and literal-with-literal-name
//     otherwise. Required-Insert-Count and Base are always 0 in the
//     emitted prefix.
//   - Decoder: full dynamic-table support. The `Decoder` type
//     (`dynamic.go`) accepts encoder-stream Insert With Name Ref /
//     Insert With Literal Name / Set Dynamic Table Capacity /
//     Duplicate instructions, resolves dynamic + post-base references
//     in field sections, detects blocked streams per
//     QPACK_BLOCKED_STREAMS, and emits Section Acknowledgment +
//     Insert Count Increment + Stream Cancellation on the decoder
//     stream.
//
// Not implemented: dynamic-table insertions on the encoder side
// (`Encode` stays static-only — outgoing response headers are well
// covered by the static table) and async blocked-stream handling
// (callers advertise `QPACK_BLOCKED_STREAMS=0`).
package qpack

// StaticEntry is one row of the RFC 9204 Appendix A static table.
type StaticEntry struct {
	Name, Value string
}

// StaticTable is the 99-row QPACK static table.
var StaticTable = [...]StaticEntry{
	{":authority", ""},
	{":path", "/"},
	{"age", "0"},
	{"content-disposition", ""},
	{"content-length", "0"},
	{"cookie", ""},
	{"date", ""},
	{"etag", ""},
	{"if-modified-since", ""},
	{"if-none-match", ""},
	{"last-modified", ""},
	{"link", ""},
	{"location", ""},
	{"referer", ""},
	{"set-cookie", ""},
	{":method", "CONNECT"},
	{":method", "DELETE"},
	{":method", "GET"},
	{":method", "HEAD"},
	{":method", "OPTIONS"},
	{":method", "POST"},
	{":method", "PUT"},
	{":scheme", "http"},
	{":scheme", "https"},
	{":status", "103"},
	{":status", "200"},
	{":status", "304"},
	{":status", "404"},
	{":status", "503"},
	{"accept", "*/*"},
	{"accept", "application/dns-message"},
	{"accept-encoding", "gzip, deflate, br"},
	{"accept-ranges", "bytes"},
	{"access-control-allow-headers", "cache-control"},
	{"access-control-allow-headers", "content-type"},
	{"access-control-allow-origin", "*"},
	{"cache-control", "max-age=0"},
	{"cache-control", "max-age=2592000"},
	{"cache-control", "max-age=604800"},
	{"cache-control", "no-cache"},
	{"cache-control", "no-store"},
	{"cache-control", "public, max-age=31536000"},
	{"content-encoding", "br"},
	{"content-encoding", "gzip"},
	{"content-type", "application/dns-message"},
	{"content-type", "application/javascript"},
	{"content-type", "application/json"},
	{"content-type", "application/x-www-form-urlencoded"},
	{"content-type", "image/gif"},
	{"content-type", "image/jpeg"},
	{"content-type", "image/png"},
	{"content-type", "text/css"},
	{"content-type", "text/html; charset=utf-8"},
	{"content-type", "text/plain"},
	{"content-type", "text/plain;charset=utf-8"},
	{"range", "bytes=0-"},
	{"strict-transport-security", "max-age=31536000"},
	{"strict-transport-security", "max-age=31536000; includesubdomains"},
	{"strict-transport-security", "max-age=31536000; includesubdomains; preload"},
	{"vary", "accept-encoding"},
	{"vary", "origin"},
	{"x-content-type-options", "nosniff"},
	{"x-xss-protection", "1; mode=block"},
	{":status", "100"},
	{":status", "204"},
	{":status", "206"},
	{":status", "302"},
	{":status", "400"},
	{":status", "403"},
	{":status", "421"},
	{":status", "425"},
	{":status", "500"},
	{"accept-language", ""},
	{"access-control-allow-credentials", "FALSE"},
	{"access-control-allow-credentials", "TRUE"},
	{"access-control-allow-headers", "*"},
	{"access-control-allow-methods", "get"},
	{"access-control-allow-methods", "get, post, options"},
	{"access-control-allow-methods", "options"},
	{"access-control-expose-headers", "content-length"},
	{"access-control-request-headers", "content-type"},
	{"access-control-request-method", "get"},
	{"access-control-request-method", "post"},
	{"alt-svc", "clear"},
	{"authorization", ""},
	{"content-security-policy", "script-src 'none'; object-src 'none'; base-uri 'none'"},
	{"early-data", "1"},
	{"expect-ct", ""},
	{"forwarded", ""},
	{"if-range", ""},
	{"origin", ""},
	{"purpose", "prefetch"},
	{"server", ""},
	{"timing-allow-origin", "*"},
	{"upgrade-insecure-requests", "1"},
	{"user-agent", ""},
	{"x-forwarded-for", ""},
	{"x-frame-options", "deny"},
	{"x-frame-options", "sameorigin"},
}

// staticIndex maps (name, value) → static index. Built lazily.
var staticByNameValue map[string]int
var staticByName map[string]int

func init() {
	staticByNameValue = make(map[string]int, len(StaticTable))
	staticByName = make(map[string]int, len(StaticTable))
	for i, e := range StaticTable {
		key := e.Name + "\x00" + e.Value
		if _, ok := staticByNameValue[key]; !ok {
			staticByNameValue[key] = i
		}
		if _, ok := staticByName[e.Name]; !ok {
			staticByName[e.Name] = i
		}
	}
}
