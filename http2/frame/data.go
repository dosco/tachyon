package frame

import "errors"

// Data carries a parsed DATA payload, with the padding already stripped.
type Data struct {
	Body      []byte // aliases into source — zero-copy
	EndStream bool
}

// ReadData parses a DATA frame payload, honoring the optional pad-length
// byte when FlagDataPadded is set. The returned Body aliases payload —
// caller is responsible for copying if needed.
func ReadData(flags Flag, payload []byte) (Data, error) {
	if !flags.Has(FlagDataPadded) {
		return Data{Body: payload, EndStream: flags.Has(FlagDataEndStream)}, nil
	}
	if len(payload) == 0 {
		return Data{}, errors.New("http2: padded DATA missing pad-length byte")
	}
	padLen := int(payload[0])
	if padLen >= len(payload) {
		return Data{}, errors.New("http2: padded DATA pad-length >= payload")
	}
	body := payload[1 : len(payload)-padLen]
	return Data{Body: body, EndStream: flags.Has(FlagDataEndStream)}, nil
}

// AppendData writes a non-padded DATA frame. Padding is a DoS surface we
// don't opt into on the send path.
func AppendData(buf []byte, streamID uint32, endStream bool, body []byte) []byte {
	var flags Flag
	if endStream {
		flags = FlagDataEndStream
	}
	buf = AppendHeader(buf, Header{
		Length: uint32(len(body)), Type: TypeData, Flags: flags, StreamID: streamID,
	})
	return append(buf, body...)
}
