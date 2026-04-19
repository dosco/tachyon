// huffman_decode.go: RFC 7541 Huffman decoder.
//
// Nibble-stride state machine. Each state corresponds to a node in the
// Huffman code tree. For every (state, 4-bit nibble) pair we precompute:
//
//   - the next state after consuming those 4 bits
//   - up to one emitted symbol (HPACK's shortest code is 5 bits, so at most
//     one symbol can complete per nibble)
//   - a fail flag for transitions that hit EOS (decoder error per RFC)
//   - an accept flag: true if the state is a valid stopping point, meaning
//     it's on the all-ones path that forms a prefix of EOS padding
//
// Table built lazily via sync.Once to avoid init() side effects.
//
// Raw (code, len) pairs live in huffman_table.go (.rodata).

package hpack

import (
	"errors"
	"sync"
)

// ErrHuffman is returned when a Huffman-encoded input is malformed (EOS
// encountered, oversized padding, or non-padding trailing bits).
var ErrHuffman = errors.New("hpack: invalid Huffman-encoded data")

// ErrHuffmanBuf is returned when the caller-provided dst is too small.
var ErrHuffmanBuf = errors.New("hpack: Huffman output buffer too small")

type huffDecNode struct {
	next   uint16
	sym    uint16
	hasSym bool
	fail   bool
	accept bool
}

var (
	huffDecOnce  sync.Once
	huffDecTable [][16]huffDecNode
)

func buildHuffDecTable() {
	// Build the bit tree from the (code, len) pairs.
	type node struct {
		sym      int // -1 if not terminal
		children [2]int
	}
	nodes := make([]node, 1, 1024)
	nodes[0] = node{sym: -1}
	for s := 0; s < 257; s++ {
		code := huffCodes[s]
		ln := huffLens[s]
		cur := 0
		for i := int(ln) - 1; i >= 0; i-- {
			bit := (code >> uint(i)) & 1
			ch := nodes[cur].children[bit]
			if ch == 0 {
				nodes = append(nodes, node{sym: -1})
				ch = len(nodes) - 1
				nodes[cur].children[bit] = ch
			}
			cur = ch
		}
		nodes[cur].sym = s
	}

	// Every internal (non-leaf) tree node becomes a decoder state. Root=0.
	stateOf := make(map[int]int)
	stateOf[0] = 0
	queue := []int{0}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, ch := range nodes[n].children {
			if ch == 0 {
				continue
			}
			if nodes[ch].sym == -1 {
				if _, ok := stateOf[ch]; !ok {
					stateOf[ch] = len(stateOf)
					queue = append(queue, ch)
				}
			}
		}
	}

	// A state is an acceptable stopping point iff it's on the all-ones
	// path from the root (i.e. the bits consumed so far to reach it are
	// all 1s). At end-of-stream the remaining padding must be a prefix of
	// EOS == all-ones, so only states lying on the 1-spine accept.
	onesPath := map[int]bool{0: true}
	cur := 0
	for {
		ch := nodes[cur].children[1]
		if ch == 0 || nodes[ch].sym != -1 {
			break
		}
		onesPath[ch] = true
		cur = ch
	}

	table := make([][16]huffDecNode, len(stateOf))
	for nodeIdx, st := range stateOf {
		for nib := 0; nib < 16; nib++ {
			cur := nodeIdx
			emitted := -1
			fail := false
			for b := 3; b >= 0; b-- {
				bit := (nib >> uint(b)) & 1
				ch := nodes[cur].children[bit]
				if ch == 0 {
					fail = true
					break
				}
				if nodes[ch].sym != -1 {
					if nodes[ch].sym == 256 {
						fail = true
						break
					}
					emitted = nodes[ch].sym
					cur = 0
				} else {
					cur = ch
				}
			}
			entry := huffDecNode{
				next:   uint16(stateOf[cur]),
				accept: onesPath[cur],
				fail:   fail,
			}
			if emitted >= 0 {
				entry.sym = uint16(emitted)
				entry.hasSym = true
			}
			table[st][nib] = entry
		}
	}
	huffDecTable = table
}

// HuffmanDecode decodes src into dst and returns the number of decoded
// bytes. No allocation; dst must be pre-sized by the caller.
func HuffmanDecode(dst, src []byte) (int, error) {
	huffDecOnce.Do(buildHuffDecTable)
	n := 0
	state := uint16(0)
	accept := true
	for _, b := range src {
		for _, nib := range [2]uint8{(b >> 4) & 0xf, b & 0xf} {
			e := huffDecTable[state][nib]
			if e.fail {
				return n, ErrHuffman
			}
			if e.hasSym {
				if n >= len(dst) {
					return n, ErrHuffmanBuf
				}
				dst[n] = byte(e.sym)
				n++
			}
			state = e.next
			accept = e.accept
		}
	}
	if !accept {
		return n, ErrHuffman
	}
	return n, nil
}
