package frame

import (
	"bytes"
	"testing"
)

func TestStreamRoundTrip(t *testing.T) {
	in := Stream{StreamID: 4, Offset: 100, Data: []byte("hello stream"), Fin: true}
	buf := AppendStream(nil, in)

	var got Stream
	err := Parse(buf, Visitor{OnStream: func(s Stream) error { got = s; return nil }})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.StreamID != in.StreamID || got.Offset != in.Offset ||
		!bytes.Equal(got.Data, in.Data) || got.Fin != in.Fin {
		t.Fatalf("stream mismatch: %+v vs %+v", got, in)
	}
}

func TestResetStreamRoundTrip(t *testing.T) {
	in := ResetStream{StreamID: 8, ErrorCode: 0x101, FinalSize: 4096}
	buf := AppendResetStream(nil, in)
	var got ResetStream
	err := Parse(buf, Visitor{OnResetStream: func(r ResetStream) error { got = r; return nil }})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != in {
		t.Fatalf("%+v vs %+v", got, in)
	}
}

func TestMaxStreamDataRoundTrip(t *testing.T) {
	in := MaxStreamData{StreamID: 12, Max: 1 << 20}
	buf := AppendMaxStreamData(nil, in)
	var got MaxStreamData
	err := Parse(buf, Visitor{OnMaxStreamData: func(m MaxStreamData) error { got = m; return nil }})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got != in {
		t.Fatalf("%+v vs %+v", got, in)
	}
}

func TestMaxStreamsRoundTrip(t *testing.T) {
	buf := AppendMaxStreams(nil, MaxStreams{Bidi: true, Max: 100})
	var got MaxStreams
	err := Parse(buf, Visitor{OnMaxStreams: func(m MaxStreams) error { got = m; return nil }})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !got.Bidi || got.Max != 100 {
		t.Fatalf("%+v", got)
	}
}

func TestHandshakeDoneFrame(t *testing.T) {
	buf := AppendHandshakeDone(nil)
	var saw bool
	err := Parse(buf, Visitor{OnHandshakeDone: func() error { saw = true; return nil }})
	if err != nil || !saw {
		t.Fatalf("saw=%v err=%v", saw, err)
	}
}
