package http1

import (
	"bytes"
	"strings"
	"testing"
)

// TestAppendChunkSizeRoundTrip ensures AppendChunkSize produces a line
// that ChunkHeader re-parses to the same size. Guards the refactor
// that split AppendChunk into AppendChunkSize + payload + CRLF.
func TestAppendChunkSizeRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 15, 16, 255, 4096, 65536, 1 << 20} {
		line := AppendChunkSize(nil, n)
		size, off, err := ChunkHeader(line)
		if err != nil {
			t.Fatalf("n=%d: ChunkHeader(%q) err=%v", n, line, err)
		}
		if int(size) != n {
			t.Fatalf("n=%d: got size=%d", n, size)
		}
		if off != len(line) {
			t.Fatalf("n=%d: off=%d want %d", n, off, len(line))
		}
	}
}

// TestReframeLikeH2Handler simulates the h2 handler's three-write
// reframing pattern (size line, payload, CRLF + last-chunk) and
// verifies CopyChunkedBody accepts the resulting stream — the same
// bytes the H2 handler will write to an upstream.
func TestReframeLikeH2Handler(t *testing.T) {
	chunks := [][]byte{
		[]byte("hello "),
		[]byte("world"),
		[]byte(strings.Repeat("x", 8000)), // 8 KiB chunk typical of H2 DATA
	}
	var framed bytes.Buffer
	for _, c := range chunks {
		framed.Write(AppendChunkSize(nil, len(c)))
		framed.Write(c)
		framed.Write(CRLF)
	}
	framed.Write(LastChunk(nil))

	// Verify re-parse by CopyChunkedBody matches the original payload
	// minus framing.
	var decoded bytes.Buffer
	scratch := make([]byte, 256)
	if err := CopyChunkedBody(&decoded, &framed, nil, scratch); err != nil {
		t.Fatalf("CopyChunkedBody: %v", err)
	}
	// CopyChunkedBody does a validating pass-through: the output
	// equals the original framed bytes. That's not a content test
	// per se; it's a confirmation that the H2-side framing is
	// well-formed by our own parser.
	// Re-run against the decoded bytes to double-check.
	reparsed, err := stripFraming(decoded.Bytes())
	if err != nil {
		t.Fatalf("stripFraming: %v", err)
	}
	var want bytes.Buffer
	for _, c := range chunks {
		want.Write(c)
	}
	if !bytes.Equal(reparsed, want.Bytes()) {
		t.Fatalf("content mismatch after strip:\n got=%q\nwant=%q",
			reparsed[:min(32, len(reparsed))],
			want.Bytes()[:min(32, want.Len())])
	}
}

// stripFraming decodes a chunked body to its raw payload bytes for
// content comparison in tests.
func stripFraming(src []byte) ([]byte, error) {
	var out []byte
	for len(src) > 0 {
		size, off, err := ChunkHeader(src)
		if err != nil {
			return nil, err
		}
		src = src[off:]
		if size == 0 {
			return out, nil
		}
		out = append(out, src[:size]...)
		src = src[size+2:] // payload + CRLF
	}
	return out, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
