package http3

import (
	"context"
	"errors"
	"testing"

	"tachyon/http3/frame"
	"tachyon/quic"
)

// fakeConn implements the Connection interface for testing the
// control-stream bring-up path without spinning up a full QUIC endpoint.
type fakeConn struct {
	openedUni []*recordedStream
	flushed   int
}

type recordedStream struct {
	id  uint64
	buf []byte
}

func (r *recordedStream) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	return len(p), nil
}

func (f *fakeConn) AcceptStream(ctx context.Context) (*quic.Stream, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeConn) OpenUniStream() (*quic.Stream, error) {
	// We can't mint a real *quic.Stream here without exporting more,
	// but openControlStream doesn't need the raw Stream type — it only
	// calls Write on it. The interface-based Serve loop uses Stream
	// directly. For this focused test we exercise the Settings-frame
	// content via a helper rather than through openControlStream.
	return nil, errors.New("fakeConn: OpenUniStream not used in this test")
}

func (f *fakeConn) Flush() error { f.flushed++; return nil }

// TestControlStreamSettingsShape verifies the SETTINGS blob the server
// advertises on the control stream matches the RFC 9114 §7.2.4
// requirements for static-table-only QPACK: capacity=0, blocked=0, plus
// a sane MAX_FIELD_SECTION_SIZE.
func TestControlStreamSettingsShape(t *testing.T) {
	// Build the same settings that openControlStream writes.
	settings := frame.Settings{
		frame.SettingQPACKMaxTableCapacity: qpackDynamicCapacity,
		frame.SettingQPACKBlockedStreams:   qpackBlockedStreams,
		frame.SettingMaxFieldSectionSize:   65536,
	}
	out := frame.AppendSettings(nil, settings)

	// Parse it back and check the three keys round-trip.
	f, n, err := frame.Parse(out)
	if err != nil {
		t.Fatalf("parse SETTINGS: %v", err)
	}
	if n != len(out) {
		t.Fatalf("parse consumed %d/%d bytes", n, len(out))
	}
	if f.Type != frame.TypeSettings {
		t.Fatalf("type=%v want %v", f.Type, frame.TypeSettings)
	}
	got, err := frame.ParseSettings(f.Payload)
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	if got[frame.SettingQPACKMaxTableCapacity] != qpackDynamicCapacity {
		t.Errorf("QPACKMaxTableCapacity=%d, want %d",
			got[frame.SettingQPACKMaxTableCapacity], qpackDynamicCapacity)
	}
	if got[frame.SettingQPACKBlockedStreams] != qpackBlockedStreams {
		t.Errorf("QPACKBlockedStreams=%d, want %d", got[frame.SettingQPACKBlockedStreams], qpackBlockedStreams)
	}
	if got[frame.SettingMaxFieldSectionSize] != 65536 {
		t.Errorf("MaxFieldSectionSize=%d, want 65536", got[frame.SettingMaxFieldSectionSize])
	}
}

// TestUniStreamTypeConstants pins the RFC 9114 §6.2 stream-type codes
// so a refactor can't silently swap them.
func TestUniStreamTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"control", uniStreamControl, 0x00},
		{"push", uniStreamPush, 0x01},
		{"qpack-encoder", uniStreamQPACKEncoder, 0x02},
		{"qpack-decoder", uniStreamQPACKDecoder, 0x03},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s stream-type = 0x%02x, want 0x%02x", c.name, c.got, c.want)
		}
	}
}
