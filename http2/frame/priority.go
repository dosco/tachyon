package frame

import "errors"

// Priority is declared for completeness. RFC 9113 deprecates it; we parse
// it to advance the stream cursor but otherwise ignore its contents.
type Priority struct {
	// Deliberately empty — field placeholder for future diagnostics.
}

// ReadPriority validates the 5-byte payload length and discards contents.
// Returning an error lets the conn surface it as a PROTOCOL_ERROR.
func ReadPriority(payload []byte) (Priority, error) {
	if len(payload) != 5 {
		return Priority{}, errors.New("http2: PRIORITY payload length != 5")
	}
	return Priority{}, nil
}
