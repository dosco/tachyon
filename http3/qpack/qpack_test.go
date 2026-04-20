package qpack

import (
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := []Field{
		{":method", "GET"},
		{":scheme", "https"},
		{":authority", "example.com"},
		{":path", "/"},
		{"user-agent", "tachyon-test/1"},
		{"accept", "*/*"},
	}
	buf := Encode(nil, in)
	out, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len mismatch: %d vs %d; out=%+v", len(out), len(in), out)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("field %d: got %+v want %+v", i, out[i], in[i])
		}
	}
}

func TestEncodeStatusResponse(t *testing.T) {
	in := []Field{
		{":status", "200"},
		{"content-type", "text/plain"},
		{"content-length", "5"},
	}
	buf := Encode(nil, in)
	out, err := Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 3 || out[0].Value != "200" || out[1].Name != "content-type" {
		t.Fatalf("got %+v", out)
	}
}
