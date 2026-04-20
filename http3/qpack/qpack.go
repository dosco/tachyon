package qpack

import (
	"errors"
	"fmt"

	"tachyon/http2/hpack"
)

// Field is a single HTTP header field.
type Field struct {
	Name, Value string
}

// Common errors.
var (
	ErrTruncated   = errors.New("qpack: truncated block")
	ErrBadRefIndex = errors.New("qpack: static-table index out of range")
)

// Encode emits a QPACK field-section-prefix of (RequiredInsertCount=0,
// Base=0) followed by one encoded representation per field. Fields that
// match a (name,value) entry in the static table are emitted as indexed;
// those that match only a name get literal-with-static-name-reference;
// otherwise both name and value are emitted as raw literal strings.
//
// No dynamic table entries are ever emitted. The prefix always signals
// "no blocking required".
func Encode(dst []byte, fields []Field) []byte {
	// Section prefix: encoded required insert count (0) and base delta (0).
	// RFC 9204 §4.5.1: RIC=0 → encoded as 0; §4.5.1.2: Base=0 → S=0, DB=0.
	dst = appendQPACKInt(dst, 0, 8, 0) // prefix for RIC
	dst = appendQPACKInt(dst, 0, 7, 0) // prefix for Base (S bit = 0)

	for _, f := range fields {
		// Try full (name,value) static match first.
		if idx, ok := staticByNameValue[f.Name+"\x00"+f.Value]; ok {
			// Indexed field line — static — §4.5.2. Pattern 1TTTxxxx with T=1.
			dst = appendQPACKInt(dst, 0b1100_0000, 6, uint64(idx))
			continue
		}
		if idx, ok := staticByName[f.Name]; ok {
			// Literal with name reference — static. §4.5.4. Pattern 01NTxxxx
			// with N=0, T=1.
			dst = appendQPACKInt(dst, 0b0101_0000, 4, uint64(idx))
			dst = appendStringLiteral(dst, f.Value)
			continue
		}
		// Literal with literal name. §4.5.6. Pattern 001NHxxx. N=0, H=0.
		dst = appendQPACKInt(dst, 0b0010_0000, 3, uint64(len(f.Name)))
		dst = append(dst, f.Name...)
		dst = appendStringLiteral(dst, f.Value)
	}
	return dst
}

// Decode parses a QPACK-encoded field section. Only the representations
// emitted by Encode plus a few common client-side ones are implemented;
// dynamic-table references return an error.
func Decode(block []byte) ([]Field, error) {
	// Section prefix: required insert count (8-bit prefix), base (7-bit
	// prefix with S bit). We ignore values and only advance past them.
	_, n, err := readQPACKInt(block, 8)
	if err != nil {
		return nil, err
	}
	block = block[n:]
	if len(block) == 0 {
		return nil, ErrTruncated
	}
	// S bit is block[0]&0x80 — ignored; delta base is 7-bit prefix.
	_, n, err = readQPACKInt(block, 7)
	if err != nil {
		return nil, err
	}
	block = block[n:]

	var fields []Field
	for len(block) > 0 {
		b := block[0]
		switch {
		case b&0b1000_0000 != 0:
			// Indexed field line. T bit = b&0x40.
			if b&0b0100_0000 == 0 {
				return nil, fmt.Errorf("qpack: dynamic-table indexed field not supported")
			}
			idx, n, err := readQPACKInt(block, 6)
			if err != nil {
				return nil, err
			}
			if int(idx) >= len(StaticTable) {
				return nil, ErrBadRefIndex
			}
			e := StaticTable[idx]
			fields = append(fields, Field{Name: e.Name, Value: e.Value})
			block = block[n:]
		case b&0b1100_0000 == 0b0100_0000:
			// Literal with name reference. N=b&0x20, T=b&0x10.
			if b&0b0001_0000 == 0 {
				return nil, fmt.Errorf("qpack: dynamic-table literal name-ref not supported")
			}
			idx, n, err := readQPACKInt(block, 4)
			if err != nil {
				return nil, err
			}
			if int(idx) >= len(StaticTable) {
				return nil, ErrBadRefIndex
			}
			block = block[n:]
			val, rest, err := readStringLiteral(block)
			if err != nil {
				return nil, err
			}
			fields = append(fields, Field{Name: StaticTable[idx].Name, Value: val})
			block = rest
		case b&0b1110_0000 == 0b0010_0000:
			// Literal with literal name. N=b&0x10, H=b&0x08.
			nameHuff := b&0b0000_1000 != 0
			nameLen, nAdv, err := readQPACKInt(block, 3)
			if err != nil {
				return nil, err
			}
			block = block[nAdv:]
			if uint64(len(block)) < nameLen {
				return nil, ErrTruncated
			}
			rawName := block[:nameLen]
			block = block[nameLen:]
			var name string
			if nameHuff {
				dst := make([]byte, 0, len(rawName)*8/5+8)
				out, err := huffmanDecode(dst, rawName)
				if err != nil {
					return nil, err
				}
				name = string(out)
			} else {
				name = string(rawName)
			}
			val, rest, err := readStringLiteral(block)
			if err != nil {
				return nil, err
			}
			fields = append(fields, Field{Name: name, Value: val})
			block = rest
		default:
			return nil, fmt.Errorf("qpack: unsupported representation 0x%02x", b)
		}
	}
	return fields, nil
}

// appendQPACKInt encodes an integer using the RFC 7541 §5.1 prefix
// algorithm (which QPACK reuses unchanged). The top bits of firstByte
// above the prefix width are preserved.
func appendQPACKInt(dst []byte, firstByte byte, n uint8, i uint64) []byte {
	max := uint64(1<<n - 1)
	if i < max {
		return append(dst, firstByte|byte(i))
	}
	dst = append(dst, firstByte|byte(max))
	i -= max
	for i >= 128 {
		dst = append(dst, byte(i&0x7f|0x80))
		i >>= 7
	}
	return append(dst, byte(i))
}

// readQPACKInt is the mirror of appendQPACKInt. n is the prefix width
// in bits.
func readQPACKInt(b []byte, n uint8) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, ErrTruncated
	}
	max := uint64(1<<n - 1)
	v := uint64(b[0]) & max
	if v < max {
		return v, 1, nil
	}
	m := uint64(0)
	i := 1
	for {
		if i >= len(b) {
			return 0, 0, ErrTruncated
		}
		x := uint64(b[i])
		v += (x & 0x7f) << m
		i++
		if x&0x80 == 0 {
			return v, i, nil
		}
		m += 7
		if m > 63 {
			return 0, 0, fmt.Errorf("qpack: varint overflow")
		}
	}
}

// appendStringLiteral encodes a string with the H-bit-prefixed length
// representation used by QPACK literal-with-name-reference value
// fields (7-bit prefix).
func appendStringLiteral(dst []byte, s string) []byte {
	// Always Huffman-encode if it saves bytes.
	enc := hpack.HuffmanEncodedLen([]byte(s))
	if enc < len(s) {
		dst = appendQPACKInt(dst, 0x80, 7, uint64(enc))
		return hpack.HuffmanEncode(dst, []byte(s))
	}
	dst = appendQPACKInt(dst, 0x00, 7, uint64(len(s)))
	return append(dst, s...)
}

func readStringLiteral(b []byte) (string, []byte, error) {
	if len(b) == 0 {
		return "", nil, ErrTruncated
	}
	huff := b[0]&0x80 != 0
	length, n, err := readQPACKInt(b, 7)
	if err != nil {
		return "", nil, err
	}
	b = b[n:]
	if uint64(len(b)) < length {
		return "", nil, ErrTruncated
	}
	raw := b[:length]
	b = b[length:]
	if !huff {
		return string(raw), b, nil
	}
	dst := make([]byte, 0, int(length)*8/5+8)
	out, err := huffmanDecode(dst, raw)
	if err != nil {
		return "", nil, err
	}
	return string(out), b, nil
}

func huffmanDecode(dst, src []byte) ([]byte, error) {
	// hpack.HuffmanDecode writes into dst and returns n; grow dst as
	// needed. We over-allocate in callers so this loop iterates at most
	// once under normal conditions.
	for {
		n, err := hpack.HuffmanDecode(dst[:cap(dst)], src)
		if err == nil {
			return dst[:n], nil
		}
		if errors.Is(err, hpack.ErrHuffmanBuf) {
			dst = make([]byte, 0, cap(dst)*2+16)
			continue
		}
		return nil, err
	}
}
