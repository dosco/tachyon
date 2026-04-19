package frame

// Ping carries the 8-byte opaque payload per RFC 7540 §6.7.
type Ping struct {
	Ack  bool
	Data [8]byte
}

// ReadPing parses an 8-byte PING payload.
func ReadPing(flags Flag, payload []byte) (Ping, bool) {
	if len(payload) != 8 {
		return Ping{}, false
	}
	var p Ping
	p.Ack = flags.Has(FlagPingAck)
	copy(p.Data[:], payload)
	return p, true
}

// AppendPing emits a PING frame — used almost exclusively to reply with
// an ACK echoing the caller's data.
func AppendPing(buf []byte, ack bool, data [8]byte) []byte {
	var flags Flag
	if ack {
		flags = FlagPingAck
	}
	buf = AppendHeader(buf, Header{
		Length: 8, Type: TypePing, Flags: flags, StreamID: 0,
	})
	return append(buf, data[:]...)
}
