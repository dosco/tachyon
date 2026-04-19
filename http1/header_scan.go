package http1

// toLower maps ASCII A-Z to a-z; leaves other bytes alone. Used for
// case-insensitive header-name comparison. Branchless on the hot path.
func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// EqualFold reports whether a and b are equal under ASCII case folding.
// Both may be any case; for our use, b is typically a lowercase constant
// (HdrHost etc) so the compiler/CPU gets a predictable comparison.
func EqualFold(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if toLower(a[i]) != toLower(b[i]) {
			return false
		}
	}
	return true
}

// isTokenChar reports whether c is a valid token byte per RFC 7230.
// Tokens appear as header field-names and as methods. We use a 128-byte
// lookup table for speed; anything >= 128 is not a token character.
var tokenTbl = [256]bool{}

func init() {
	// tchar = "!" / "#" / "$" / "%" / "&" / "'" / "*"
	//       / "+" / "-" / "." / "^" / "_" / "`" / "|" / "~"
	//       / DIGIT / ALPHA
	for c := byte('0'); c <= '9'; c++ {
		tokenTbl[c] = true
	}
	for c := byte('a'); c <= 'z'; c++ {
		tokenTbl[c] = true
	}
	for c := byte('A'); c <= 'Z'; c++ {
		tokenTbl[c] = true
	}
	for _, c := range []byte("!#$%&'*+-.^_`|~") {
		tokenTbl[c] = true
	}
}

func isTokenChar(c byte) bool { return tokenTbl[c] }

// trimOWS returns p with leading/trailing optional whitespace removed
// (RFC 7230: SP / HTAB). Returns a sub-slice of p; no allocation.
func trimOWS(p []byte) []byte {
	i, j := 0, len(p)
	for i < j && (p[i] == ' ' || p[i] == '\t') {
		i++
	}
	for j > i && (p[j-1] == ' ' || p[j-1] == '\t') {
		j--
	}
	return p[i:j]
}

// parseUint decodes a decimal non-negative int64 from p. Returns -1 on any
// malformed input (including empty, leading zeros are permitted for
// Content-Length compatibility). No heap use.
func parseUint(p []byte) int64 {
	if len(p) == 0 {
		return -1
	}
	var n int64
	for _, c := range p {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int64(c-'0')
		if n < 0 { // overflow
			return -1
		}
	}
	return n
}

// findCRLF returns the index of "\r\n" in p starting at i, or -1 if absent.
// Inlined by the compiler at the one hot call site.
func findCRLF(p []byte, i int) int {
	end := len(p) - 1
	for i < end {
		if p[i] == '\r' && p[i+1] == '\n' {
			return i
		}
		i++
	}
	return -1
}
