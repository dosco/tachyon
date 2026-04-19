package http1

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestCopyChunkedHappy walks the common shapes of a well-formed
// chunked body and checks byte-for-byte pass-through.
func TestCopyChunkedHappy(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		bodyHead string // portion of the body prefix already in the caller's buffer
	}{
		{
			name: "single chunk no head",
			body: "5\r\nhello\r\n0\r\n\r\n",
		},
		{
			name: "two chunks no head",
			body: "5\r\nhello\r\n5\r\nworld\r\n0\r\n\r\n",
		},
		{
			name: "body fully in head",
			body: "",
			bodyHead: "5\r\nhello\r\n0\r\n\r\n",
		},
		{
			name:     "body split between head and src",
			body:     "world\r\n0\r\n\r\n",
			bodyHead: "5\r\nhello\r\n5\r\n",
		},
		{
			name: "chunk ext ignored",
			body: "5;ext=foo\r\nhello\r\n0\r\n\r\n",
		},
		{
			name: "uppercase hex",
			body: "A\r\n0123456789\r\n0\r\n\r\n",
		},
		{
			name: "single byte chunks",
			body: "1\r\na\r\n1\r\nb\r\n1\r\nc\r\n0\r\n\r\n",
		},
	}
	scratch := make([]byte, 256)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			src := strings.NewReader(tc.body)
			err := CopyChunkedBody(&dst, src, []byte(tc.bodyHead), scratch)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			want := tc.bodyHead + tc.body
			if got := dst.String(); got != want {
				t.Fatalf("round-trip mismatch:\n got=%q\nwant=%q", got, want)
			}
		})
	}
}

// TestCopyChunkedMalformed covers the defensive paths.
func TestCopyChunkedMalformed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bad hex in size", "5x\r\nhello\r\n0\r\n\r\n"},
		{"no CRLF after payload", "5\r\nhelloXX0\r\n\r\n"},
		{"truncated size line", "5"},
		{"truncated payload", "5\r\nhel"},
		{"missing final CRLF", "0\r\n"},
		{"trailer header present (unsupported)", "0\r\nTrailer: x\r\n\r\n"},
	}
	scratch := make([]byte, 256)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			src := strings.NewReader(tc.body)
			err := CopyChunkedBody(&dst, src, nil, scratch)
			if err == nil {
				t.Fatalf("expected error, got nil; dst=%q", dst.String())
			}
			// Must be one of the framing-error classes. ErrMalformed,
			// ErrTooLarge, or io.ErrUnexpectedEOF (truncated input).
			if !errors.Is(err, ErrMalformed) &&
				!errors.Is(err, ErrTooLarge) &&
				!errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("unexpected err class: %v", err)
			}
		})
	}
}

// TestCopyChunkedLargePayload ensures the "remaining >= cap(scratch)"
// fast path reads straight into scratch and streams without a copy.
func TestCopyChunkedLargePayload(t *testing.T) {
	payload := make([]byte, 32*1024) // 32 KiB — larger than scratch (1 KiB)
	for i := range payload {
		payload[i] = byte(i & 0xff)
	}
	var body bytes.Buffer
	body.WriteString("8000\r\n") // 0x8000 = 32768
	body.Write(payload)
	body.WriteString("\r\n0\r\n\r\n")

	scratch := make([]byte, 1024)
	var dst bytes.Buffer
	err := CopyChunkedBody(&dst, &body, nil, scratch)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// Verify the payload round-tripped intact. Strip framing.
	got := dst.Bytes()
	// Expect: "8000\r\n" + payload + "\r\n0\r\n\r\n"
	const prefix = "8000\r\n"
	const suffix = "\r\n0\r\n\r\n"
	if !bytes.HasPrefix(got, []byte(prefix)) || !bytes.HasSuffix(got, []byte(suffix)) {
		t.Fatalf("framing malformed: %q ... %q", got[:16], got[len(got)-16:])
	}
	inner := got[len(prefix) : len(got)-len(suffix)]
	if !bytes.Equal(inner, payload) {
		t.Fatalf("payload corrupted: len=%d want=%d", len(inner), len(payload))
	}
}

// TestCopyChunkedSmallScratchRejected guards the cap check.
func TestCopyChunkedSmallScratchRejected(t *testing.T) {
	var dst bytes.Buffer
	scratch := make([]byte, 16)
	err := CopyChunkedBody(&dst, strings.NewReader("0\r\n\r\n"), nil, scratch)
	if err == nil {
		t.Fatalf("expected error from tiny scratch")
	}
}
