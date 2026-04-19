package frame

import (
	"encoding/binary"
	"errors"
)

// SettingID is the 16-bit SETTINGS identifier.
type SettingID uint16

const (
	SettingHeaderTableSize      SettingID = 0x1
	SettingEnablePush           SettingID = 0x2
	SettingMaxConcurrentStreams SettingID = 0x3
	SettingInitialWindowSize    SettingID = 0x4
	SettingMaxFrameSize         SettingID = 0x5
	SettingMaxHeaderListSize    SettingID = 0x6
)

// Setting is one (id, value) pair inside a SETTINGS frame.
type Setting struct {
	ID    SettingID
	Value uint32
}

// ErrMalformedSettings is returned when a SETTINGS payload is not a
// multiple of six bytes (per RFC 7540 §6.5).
var ErrMalformedSettings = errors.New("http2: SETTINGS payload length not a multiple of 6")

// ReadSettings walks payload six bytes at a time, yielding each pair to
// fn. Returning false from fn stops the walk early. No allocation — the
// caller decides how to materialize the settings.
func ReadSettings(payload []byte, fn func(Setting) bool) error {
	if len(payload)%6 != 0 {
		return ErrMalformedSettings
	}
	for i := 0; i < len(payload); i += 6 {
		s := Setting{
			ID:    SettingID(binary.BigEndian.Uint16(payload[i : i+2])),
			Value: binary.BigEndian.Uint32(payload[i+2 : i+6]),
		}
		if !fn(s) {
			return nil
		}
	}
	return nil
}

// AppendSettings writes a SETTINGS frame (header + payload) onto buf.
// Pass ack=true with a nil settings slice to emit a SETTINGS ACK.
func AppendSettings(buf []byte, ack bool, settings []Setting) []byte {
	var flags Flag
	if ack {
		flags = FlagSettingsAck
	}
	buf = AppendHeader(buf, Header{
		Length:   uint32(len(settings) * 6),
		Type:     TypeSettings,
		Flags:    flags,
		StreamID: 0,
	})
	for _, s := range settings {
		var b [6]byte
		binary.BigEndian.PutUint16(b[0:2], uint16(s.ID))
		binary.BigEndian.PutUint32(b[2:6], s.Value)
		buf = append(buf, b[:]...)
	}
	return buf
}
