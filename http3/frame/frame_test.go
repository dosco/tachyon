package frame

import (
	"bytes"
	"testing"
)

func TestDataRoundTrip(t *testing.T) {
	body := []byte("hello http3 body")
	buf := AppendFrame(nil, TypeData, body)
	f, n, err := Parse(buf)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n != len(buf) || f.Type != TypeData || !bytes.Equal(f.Payload, body) {
		t.Fatalf("got %+v n=%d", f, n)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	in := Settings{
		SettingQPACKMaxTableCapacity: 4096,
		SettingMaxFieldSectionSize:   65536,
	}
	buf := AppendSettings(nil, in)
	f, _, err := Parse(buf)
	if err != nil || f.Type != TypeSettings {
		t.Fatalf("parse: %v %+v", err, f)
	}
	got, err := ParseSettings(f.Payload)
	if err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	if got[SettingQPACKMaxTableCapacity] != 4096 || got[SettingMaxFieldSectionSize] != 65536 {
		t.Fatalf("settings mismatch: %+v", got)
	}
}

func TestGoAwayRoundTrip(t *testing.T) {
	buf := AppendGoAway(nil, GoAway{StreamOrPushID: 12})
	f, _, err := Parse(buf)
	if err != nil || f.Type != TypeGoAway {
		t.Fatalf("parse: %v %+v", err, f)
	}
}
