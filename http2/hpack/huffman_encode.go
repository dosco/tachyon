// huffman_encode.go: RFC 7541 Huffman encoder.
//
// Accumulates bits into a uint64 and flushes bytes when at least 8 bits are
// buffered. Final partial byte is padded with the high bits of EOS (i.e.
// all-ones), per RFC 7541 §5.2.
//
// Zero allocations: callers pass a dst slice with enough capacity (use
// HuffmanEncodedLen to size it).

package hpack

// HuffmanEncodedLen returns the exact number of bytes HuffmanEncode will
// append for src. Computed from the per-byte bit lengths; the trailing
// padding rounds up to a whole byte.
func HuffmanEncodedLen(src []byte) int {
	var bits uint64
	for _, b := range src {
		bits += uint64(huffLens[b])
	}
	return int((bits + 7) / 8)
}

// HuffmanEncode appends the Huffman-encoded form of src to dst and returns
// the extended slice. Never allocates if dst has sufficient capacity.
func HuffmanEncode(dst, src []byte) []byte {
	var acc uint64 // bit accumulator, MSB-aligned at position (64-nbits)
	var nbits uint // number of valid bits currently in acc, from the top
	for _, b := range src {
		c := uint64(huffCodes[b])
		l := uint(huffLens[b])
		// Place the new code immediately below the existing bits.
		acc |= c << (64 - nbits - l)
		nbits += l
		for nbits >= 8 {
			dst = append(dst, byte(acc>>56))
			acc <<= 8
			nbits -= 8
		}
	}
	if nbits > 0 {
		// Pad remaining bits with ones (EOS high bits).
		pad := uint64(0xff) << (56)
		// Mask pad to only fill the unused low bits of the current byte.
		acc |= pad >> nbits
		dst = append(dst, byte(acc>>56))
	}
	return dst
}
