// static_table.go: RFC 7541 Appendix A predefined static table.
//
// Entries are 1-indexed per the spec; index 0 is reserved. We store as a
// package-level array of literals so the compiler places it in .rodata with
// no init-time cost and no allocation at lookup.
//
// Invariant: staticTable[0] is a zero sentinel; real data starts at [1].

package hpack

type staticEntry struct {
	name  string
	value string
}

// StaticLen is the count of real entries in the static table (RFC 7541 §A).
const StaticLen = 61

var staticTable = [StaticLen + 1]staticEntry{
	{"", ""}, // index 0 is reserved / unused
	{":authority", ""},
	{":method", "GET"},
	{":method", "POST"},
	{":path", "/"},
	{":path", "/index.html"},
	{":scheme", "http"},
	{":scheme", "https"},
	{":status", "200"},
	{":status", "204"},
	{":status", "206"},
	{":status", "304"},
	{":status", "400"},
	{":status", "404"},
	{":status", "500"},
	{"accept-charset", ""},
	{"accept-encoding", "gzip, deflate"},
	{"accept-language", ""},
	{"accept-ranges", ""},
	{"accept", ""},
	{"access-control-allow-origin", ""},
	{"age", ""},
	{"allow", ""},
	{"authorization", ""},
	{"cache-control", ""},
	{"content-disposition", ""},
	{"content-encoding", ""},
	{"content-language", ""},
	{"content-length", ""},
	{"content-location", ""},
	{"content-range", ""},
	{"content-type", ""},
	{"cookie", ""},
	{"date", ""},
	{"etag", ""},
	{"expect", ""},
	{"expires", ""},
	{"from", ""},
	{"host", ""},
	{"if-match", ""},
	{"if-modified-since", ""},
	{"if-none-match", ""},
	{"if-range", ""},
	{"if-unmodified-since", ""},
	{"last-modified", ""},
	{"link", ""},
	{"location", ""},
	{"max-forwards", ""},
	{"proxy-authenticate", ""},
	{"proxy-authorization", ""},
	{"range", ""},
	{"referer", ""},
	{"refresh", ""},
	{"retry-after", ""},
	{"server", ""},
	{"set-cookie", ""},
	{"strict-transport-security", ""},
	{"transfer-encoding", ""},
	{"user-agent", ""},
	{"vary", ""},
	{"via", ""},
	{"www-authenticate", ""},
}

// StaticEntry returns the (name, value) pair at 1-based index idx.
// ok=false for out-of-range indices.
func StaticEntry(idx int) (name, value string, ok bool) {
	if idx < 1 || idx > StaticLen {
		return "", "", false
	}
	e := staticTable[idx]
	return e.name, e.value, true
}

// staticFindExact returns the 1-based static index matching (name, value)
// exactly, or 0 if none. Linear scan; StaticLen is small and bounded.
func staticFindExact(name, value string) int {
	for i := 1; i <= StaticLen; i++ {
		if staticTable[i].name == name && staticTable[i].value == value {
			return i
		}
	}
	return 0
}

// staticFindName returns the 1-based static index of the first entry whose
// name matches, or 0 if none.
func staticFindName(name string) int {
	for i := 1; i <= StaticLen; i++ {
		if staticTable[i].name == name {
			return i
		}
	}
	return 0
}
