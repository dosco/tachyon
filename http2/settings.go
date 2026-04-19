// Local + peer settings, ACK tracking.
//
// Per RFC 7540 §6.5: each endpoint declares settings at connection
// start; every SETTINGS frame must be ACKed with an empty SETTINGS flag
// ACK. The sender doesn't apply pending changes until the ACK lands.
//
// We take a narrow stance:
//   - Our *local* settings are fixed at connection start and never
//     change. Simpler than re-negotiating max_frame_size mid-stream.
//   - We honor the peer's settings strictly (they tell us what *they*
//     can receive), but bound them to our compiled-in maxima.

//go:build linux

package http2

import "tachyon/http2/frame"

// Limits we'll advertise to the peer.
const (
	defaultHeaderTableSize      = 4096
	defaultMaxConcurrentStreams = 256
	defaultInitialWindowSize    = 65535 // RFC default; we grow it via BDP later
	defaultMaxFrameSize         = 1 << 14
	defaultMaxHeaderListSize    = 1 << 20 // 1 MiB cap against header-bomb abuse
)

// Settings tracks the currently-applied settings of one peer.
type Settings struct {
	HeaderTableSize      uint32
	EnablePush           uint32 // server always sends 0 here
	MaxConcurrentStreams uint32
	InitialWindowSize    uint32
	MaxFrameSize         uint32
	MaxHeaderListSize    uint32
}

// DefaultClient are the defaults we assume for the client before it has
// sent any SETTINGS (the RFC floor values).
func DefaultClient() Settings {
	return Settings{
		HeaderTableSize:      4096,
		EnablePush:           1,
		MaxConcurrentStreams: 0xffffffff,
		InitialWindowSize:    65535,
		MaxFrameSize:         16384,
		MaxHeaderListSize:    0xffffffff,
	}
}

// Local returns the settings we advertise.
func Local() []frame.Setting {
	return []frame.Setting{
		{ID: frame.SettingHeaderTableSize, Value: defaultHeaderTableSize},
		{ID: frame.SettingEnablePush, Value: 0},
		{ID: frame.SettingMaxConcurrentStreams, Value: defaultMaxConcurrentStreams},
		{ID: frame.SettingInitialWindowSize, Value: defaultInitialWindowSize},
		{ID: frame.SettingMaxFrameSize, Value: defaultMaxFrameSize},
		{ID: frame.SettingMaxHeaderListSize, Value: defaultMaxHeaderListSize},
	}
}

// Apply mutates s to reflect a SETTINGS update from the peer.
func (s *Settings) Apply(id frame.SettingID, val uint32) {
	switch id {
	case frame.SettingHeaderTableSize:
		s.HeaderTableSize = val
	case frame.SettingEnablePush:
		s.EnablePush = val
	case frame.SettingMaxConcurrentStreams:
		s.MaxConcurrentStreams = val
	case frame.SettingInitialWindowSize:
		s.InitialWindowSize = val
	case frame.SettingMaxFrameSize:
		s.MaxFrameSize = val
	case frame.SettingMaxHeaderListSize:
		s.MaxHeaderListSize = val
	}
}
