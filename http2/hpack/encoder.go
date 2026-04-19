// encoder.go: HPACK field-section encoder.
//
// Insertion policy (per doc.go): the only names we insert into the dynamic
// table are :status, content-type, and server. Everything else goes out as
// literal-without-indexing. Rationale: headers like date, content-length,
// and etag are per-response and evict useful entries. The narrow allow-list
// captures the compression win on the repetitive bits without churn.
//
// Strings: always sent Huffman-encoded if that's smaller than raw.
//
// All output goes through AppendField on a caller-supplied buf; no
// per-call allocation.

package hpack

// Encoder holds the encoder-side dynamic table. The encoder's table must
// mirror the peer decoder's table — one Encoder per HTTP/2 connection.
type Encoder struct {
	dt *DynamicTable
}

// NewEncoder returns an Encoder mirroring into dt.
func NewEncoder(dt *DynamicTable) *Encoder {
	return &Encoder{dt: dt}
}

// shouldIndex reports whether (name) is in the narrow insertion allow-list.
func shouldIndex(name []byte) bool {
	switch string(name) {
	case ":status", "content-type", "server":
		return true
	}
	return false
}

// AppendField appends the HPACK encoding of (name, value) to buf and
// returns the extended slice.
func (e *Encoder) AppendField(buf, name, value []byte) []byte {
	// 1) Exact static-table match -> indexed.
	if idx := staticFindExact(string(name), string(value)); idx != 0 {
		return appendIndexed(buf, uint64(idx))
	}
	// 2) Dynamic-table exact match -> indexed (cheap win even though we
	//    don't proactively insert most things; our narrow allow-list may
	//    have produced one).
	if idx := e.dynFindExact(name, value); idx != 0 {
		return appendIndexed(buf, uint64(StaticLen+idx))
	}

	// Name index for literal reuse (static OR dynamic).
	nameIdx := staticFindName(string(name))
	if nameIdx == 0 {
		if di := e.dynFindName(name); di != 0 {
			nameIdx = StaticLen + di
		}
	}

	if shouldIndex(name) && len(value) <= 64 {
		buf = appendLiteral(buf, 0x40, 6, nameIdx, name, value)
		e.dt.Add(name, value)
		return buf
	}
	// Default: literal without indexing, 4-bit prefix, flag byte 0x00.
	return appendLiteral(buf, 0x00, 4, nameIdx, name, value)
}

// AppendIndexedStatus is a fast path for status codes present in the
// static table (200/204/206/304/400/404/500).
func (e *Encoder) AppendIndexedStatus(buf []byte, status int) []byte {
	idx := 0
	switch status {
	case 200:
		idx = 8
	case 204:
		idx = 9
	case 206:
		idx = 10
	case 304:
		idx = 11
	case 400:
		idx = 12
	case 404:
		idx = 13
	case 500:
		idx = 14
	}
	if idx != 0 {
		return appendIndexed(buf, uint64(idx))
	}
	// Fallback: emit :status as a literal with the :status name index (8).
	var tmp [4]byte
	n := 0
	if status >= 100 && status <= 999 {
		tmp[n] = byte('0' + status/100)
		n++
		tmp[n] = byte('0' + (status/10)%10)
		n++
		tmp[n] = byte('0' + status%10)
		n++
	}
	return appendLiteral(buf, 0x40, 6, 8, nil, tmp[:n])
}

// dynFindExact scans the dynamic table for a (name,value) match and returns
// the 1-based dynamic index, or 0.
func (e *Encoder) dynFindExact(name, value []byte) int {
	for i := 1; i <= e.dt.Len(); i++ {
		n, v, _ := e.dt.Get(i)
		if bytesEq(n, name) && bytesEq(v, value) {
			return i
		}
	}
	return 0
}

func (e *Encoder) dynFindName(name []byte) int {
	for i := 1; i <= e.dt.Len(); i++ {
		n, _, _ := e.dt.Get(i)
		if bytesEq(n, name) {
			return i
		}
	}
	return 0
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// appendIndexed emits an indexed header field (RFC 7541 §6.1).
func appendIndexed(buf []byte, idx uint64) []byte {
	return appendInt(buf, 0x80, 7, idx)
}

// appendLiteral emits a literal representation. flag is the high bits
// (0x40 for incremental, 0x00 for without-indexing, 0x10 for never), n is
// the prefix bit count, nameIdx is the static/dynamic name index or 0 to
// inline the name as a string.
func appendLiteral(buf []byte, flag byte, n uint, nameIdx int, name, value []byte) []byte {
	if nameIdx > 0 {
		buf = appendInt(buf, flag, n, uint64(nameIdx))
	} else {
		buf = appendInt(buf, flag, n, 0)
		buf = appendString(buf, name)
	}
	buf = appendString(buf, value)
	return buf
}

// appendString emits a length-prefixed (possibly Huffman) string.
func appendString(buf, s []byte) []byte {
	hl := HuffmanEncodedLen(s)
	if hl < len(s) {
		buf = appendInt(buf, 0x80, 7, uint64(hl))
		return HuffmanEncode(buf, s)
	}
	buf = appendInt(buf, 0x00, 7, uint64(len(s)))
	return append(buf, s...)
}

// appendInt encodes v with an n-bit prefix (RFC 7541 §5.1). The high bits
// of the first byte are taken from flag (flag's low n bits must be zero).
func appendInt(buf []byte, flag byte, n uint, v uint64) []byte {
	mask := uint64(1<<n - 1)
	if v < mask {
		return append(buf, flag|byte(v))
	}
	buf = append(buf, flag|byte(mask))
	v -= mask
	for v >= 0x80 {
		buf = append(buf, byte(v&0x7f)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}
