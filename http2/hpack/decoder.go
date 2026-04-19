// decoder.go: HPACK field-section decoder.
//
// Single-pass over the block with a branch on the first byte of each
// representation (RFC 7541 §6):
//
//   0b1xxxxxxx - indexed header field                     (§6.1)
//   0b01xxxxxx - literal with incremental indexing        (§6.2.1)
//   0b001xxxxx - dynamic table size update                (§6.3)
//   0b0001xxxx - literal never indexed                    (§6.2.3)
//   0b0000xxxx - literal without indexing                 (§6.2.2)
//
// String decoding: bit 7 of the first length byte = Huffman. Raw strings
// alias the input block; Huffman strings decode into a per-Decoder scratch
// so the emitted []byte remains valid until the next field is decoded.
//
// Emitted Field.Name/Value slices MUST NOT be retained past the emit
// callback by the caller (they may alias caller scratch).

package hpack

import (
	"errors"
)

var (
	ErrDecoderTruncated = errors.New("hpack: truncated input")
	ErrInvalidIndex     = errors.New("hpack: invalid table index")
	ErrIntegerOverflow  = errors.New("hpack: integer overflow")
)

// Field is a single decoded header field. Name and Value alias either the
// input buffer, the decoder's Huffman scratch, or the dynamic-table arena.
type Field struct {
	Name, Value []byte
}

// Decoder holds state shared across multiple Decode calls on the same
// HPACK connection: the dynamic table and a Huffman scratch buffer.
type Decoder struct {
	dt      *DynamicTable
	huffBuf [2048]byte
	// nameBuf and valBuf are split halves of huffBuf used per-field so
	// that a field's name and value can simultaneously be Huffman-decoded
	// without clobbering each other.
}

// NewDecoder returns a Decoder backed by dt.
func NewDecoder(dt *DynamicTable) *Decoder {
	return &Decoder{dt: dt}
}

// Decode consumes the HPACK block and invokes emit for each decoded field.
// If emit returns false, decoding halts and returns nil. Any wire-format
// error is returned.
func (d *Decoder) Decode(block []byte, emit func(Field) bool) error {
	i := 0
	for i < len(block) {
		b := block[i]
		switch {
		case b&0x80 != 0:
			// Indexed header field.
			idx, ni, err := decodeInt(block, i, 7)
			if err != nil {
				return err
			}
			i = ni
			name, value, err := d.lookup(int(idx))
			if err != nil {
				return err
			}
			if !emit(Field{Name: name, Value: value}) {
				return nil
			}
		case b&0xc0 == 0x40:
			// Literal, incremental indexing. Prefix=6.
			idx, ni, err := decodeInt(block, i, 6)
			if err != nil {
				return err
			}
			i = ni
			name, value, ni, err := d.readLiteral(block, i, int(idx))
			if err != nil {
				return err
			}
			i = ni
			d.dt.Add(name, value)
			// After Add the arena may have moved; re-alias via the newest
			// dynamic entry so Field slices remain valid post-insert.
			n2, v2, ok := d.dt.Get(1)
			if ok {
				name, value = n2, v2
			}
			if !emit(Field{Name: name, Value: value}) {
				return nil
			}
		case b&0xe0 == 0x20:
			// Dynamic table size update.
			sz, ni, err := decodeInt(block, i, 5)
			if err != nil {
				return err
			}
			i = ni
			d.dt.SetMaxSize(int(sz))
		case b&0xf0 == 0x10:
			// Literal, never indexed. Prefix=4.
			idx, ni, err := decodeInt(block, i, 4)
			if err != nil {
				return err
			}
			i = ni
			name, value, ni, err := d.readLiteral(block, i, int(idx))
			if err != nil {
				return err
			}
			i = ni
			if !emit(Field{Name: name, Value: value}) {
				return nil
			}
		default:
			// Literal, without indexing. Prefix=4.
			idx, ni, err := decodeInt(block, i, 4)
			if err != nil {
				return err
			}
			i = ni
			name, value, ni, err := d.readLiteral(block, i, int(idx))
			if err != nil {
				return err
			}
			i = ni
			if !emit(Field{Name: name, Value: value}) {
				return nil
			}
		}
	}
	return nil
}

// lookup resolves an HPACK index to (name, value) across static+dynamic.
func (d *Decoder) lookup(idx int) (name, value []byte, err error) {
	if idx == 0 {
		return nil, nil, ErrInvalidIndex
	}
	if idx <= StaticLen {
		e := staticTable[idx]
		// Convert strings to []byte without allocation via unsafe? We
		// accept the tiny zero-length descriptor copy; the string header
		// points into .rodata so the resulting []byte aliases .rodata.
		return []byte(e.name), []byte(e.value), nil
	}
	n, v, ok := d.dt.Get(idx - StaticLen)
	if !ok {
		return nil, nil, ErrInvalidIndex
	}
	return n, v, nil
}

// readLiteral reads the (optional) name and the value of a literal
// representation starting at block[i]. nameIdx is the already-parsed name
// index (0 means a name literal follows).
func (d *Decoder) readLiteral(block []byte, i, nameIdx int) (name, value []byte, ni int, err error) {
	if nameIdx == 0 {
		name, ni, err = d.readString(block, i, d.huffBuf[:1024], 0)
		if err != nil {
			return nil, nil, 0, err
		}
		i = ni
	} else {
		name, _, err = d.lookup(nameIdx)
		if err != nil {
			return nil, nil, 0, err
		}
	}
	value, ni, err = d.readString(block, i, d.huffBuf[:2048], 1024)
	if err != nil {
		return nil, nil, 0, err
	}
	return name, value, ni, nil
}

// readString decodes a length-prefixed (possibly Huffman) string. scratch
// is the buffer for Huffman materialization; scratchOff is the offset
// within scratch to start writing so name and value can coexist.
func (d *Decoder) readString(block []byte, i int, scratch []byte, scratchOff int) ([]byte, int, error) {
	if i >= len(block) {
		return nil, 0, ErrDecoderTruncated
	}
	huff := block[i]&0x80 != 0
	n, ni, err := decodeInt(block, i, 7)
	if err != nil {
		return nil, 0, err
	}
	i = ni
	if i+int(n) > len(block) {
		return nil, 0, ErrDecoderTruncated
	}
	raw := block[i : i+int(n)]
	i += int(n)
	if !huff {
		return raw, i, nil
	}
	dst := scratch[scratchOff:]
	m, err := HuffmanDecode(dst, raw)
	if err != nil {
		return nil, 0, err
	}
	return dst[:m], i, nil
}

// decodeInt decodes a variable-length integer starting at block[i] using
// an n-bit prefix (RFC 7541 §5.1). Returns the value and new position.
func decodeInt(block []byte, i int, n uint) (uint64, int, error) {
	if i >= len(block) {
		return 0, 0, ErrDecoderTruncated
	}
	mask := byte(1<<n - 1)
	v := uint64(block[i] & mask)
	i++
	if v < uint64(mask) {
		return v, i, nil
	}
	var m uint = 0
	for i < len(block) {
		b := block[i]
		i++
		v += uint64(b&0x7f) << m
		m += 7
		if m > 63 {
			return 0, 0, ErrIntegerOverflow
		}
		if b&0x80 == 0 {
			return v, i, nil
		}
	}
	return 0, 0, ErrDecoderTruncated
}
