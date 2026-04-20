package quic

import (
	"bytes"
	"testing"
)

func TestStreamSendRoundTrip(t *testing.T) {
	s := NewStream(4)
	n, err := s.Write([]byte("hello world"))
	if err != nil || n != 11 {
		t.Fatalf("Write n=%d err=%v", n, err)
	}
	_ = s.CloseWrite()

	data, offset, fin := s.PopSend(5)
	if !bytes.Equal(data, []byte("hello")) || offset != 0 || fin {
		t.Fatalf("pop1: data=%q offset=%d fin=%v", data, offset, fin)
	}
	data, offset, fin = s.PopSend(100)
	if !bytes.Equal(data, []byte(" world")) || offset != 5 || !fin {
		t.Fatalf("pop2: data=%q offset=%d fin=%v", data, offset, fin)
	}
	// Further pops yield nothing.
	data, _, fin = s.PopSend(1)
	if data != nil || fin {
		t.Fatalf("pop3: data=%v fin=%v", data, fin)
	}
}

func TestStreamRecvInOrder(t *testing.T) {
	s := NewStream(4)
	s.OnStream(0, []byte("abc"), false)
	s.OnStream(3, []byte("defg"), true)

	buf := make([]byte, 16)
	n, fin := s.Read(buf)
	if n != 7 || !fin || string(buf[:n]) != "abcdefg" {
		t.Fatalf("read: n=%d fin=%v buf=%q", n, fin, buf[:n])
	}
}

func TestStreamRecvDuplicate(t *testing.T) {
	s := NewStream(4)
	s.OnStream(0, []byte("abc"), false)
	s.OnStream(0, []byte("abcdef"), false) // overlaps
	buf := make([]byte, 16)
	n, _ := s.Read(buf)
	if string(buf[:n]) != "abcdef" {
		t.Fatalf("got %q", buf[:n])
	}
}

func TestStreamResetWrite(t *testing.T) {
	s := NewStream(4)
	_, _ = s.Write([]byte("not-sent"))
	s.ResetWrite(0x42)
	pending, code := s.HasPendingReset()
	if !pending || code != 0x42 {
		t.Fatalf("pending=%v code=%x", pending, code)
	}
	// After reset, PopSend should yield nothing.
	data, _, _ := s.PopSend(100)
	if data != nil {
		t.Fatalf("post-reset PopSend returned %q", data)
	}
}
