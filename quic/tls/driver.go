// Package tls wraps crypto/tls.QUICConn to give the QUIC endpoint a
// tighter, event-oriented handshake driver.
//
// The stdlib API is already minimal — this package exists mainly to
// centralize the event-drain loop, to keep the endpoint free of raw
// crypto/tls symbols, and to surface an Event union that maps cleanly
// onto the packet-level actions the endpoint needs to take (install
// keys, send a CRYPTO frame at a given encryption level, mark the
// handshake complete).
package tls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
)

// Level is the QUIC encryption level at which a handshake message is
// carried.
type Level int

const (
	LevelInitial Level = iota
	LevelEarly
	LevelHandshake
	LevelApplication
)

func fromStdLevel(l tls.QUICEncryptionLevel) Level {
	switch l {
	case tls.QUICEncryptionLevelInitial:
		return LevelInitial
	case tls.QUICEncryptionLevelEarly:
		return LevelEarly
	case tls.QUICEncryptionLevelHandshake:
		return LevelHandshake
	case tls.QUICEncryptionLevelApplication:
		return LevelApplication
	default:
		return LevelInitial
	}
}

func toStdLevel(l Level) tls.QUICEncryptionLevel {
	switch l {
	case LevelInitial:
		return tls.QUICEncryptionLevelInitial
	case LevelEarly:
		return tls.QUICEncryptionLevelEarly
	case LevelHandshake:
		return tls.QUICEncryptionLevelHandshake
	case LevelApplication:
		return tls.QUICEncryptionLevelApplication
	}
	return tls.QUICEncryptionLevelInitial
}

// EventKind identifies the kind of handshake event emitted by the TLS
// driver.
type EventKind int

const (
	EventNone EventKind = iota
	EventSetReadSecret
	EventSetWriteSecret
	EventWriteData
	EventTransportParameters
	EventTransportParametersRequired
	EventHandshakeComplete
)

// Event is one item drained from the TLS handshake state machine.
type Event struct {
	Kind  EventKind
	Level Level
	Suite uint16 // set for key-install events
	Data  []byte // set for write-data, transport params, key install
}

// Conn is a handshake-driving wrapper around tls.QUICConn.
type Conn struct {
	q      *tls.QUICConn
	cfg    *tls.QUICConfig
	closed bool
}

// NewServer builds a server-side driver. The caller must provide a
// *tls.Config whose Certificates (or GetCertificate) and NextProtos are
// already set up; ALPN in NextProtos should include "h3".
func NewServer(base *tls.Config) *Conn {
	cfg := &tls.QUICConfig{TLSConfig: base}
	return &Conn{q: tls.QUICServer(cfg), cfg: cfg}
}

// SetTransportParameters installs the local QUIC transport parameters
// that TLS will carry in the encrypted extensions. Call before Start.
func (c *Conn) SetTransportParameters(params []byte) {
	c.q.SetTransportParameters(params)
}

// Start kicks off the handshake state machine. The first batch of
// events (typically a QUICTransportParametersRequired) will be
// available via Events() after this returns.
func (c *Conn) Start(ctx context.Context) error {
	return c.q.Start(ctx)
}

// HandleCrypto feeds decrypted CRYPTO-frame bytes at the given level
// into the TLS state machine. After calling this the caller should
// drain events with Events().
func (c *Conn) HandleCrypto(level Level, data []byte) error {
	if c.closed {
		return errors.New("quic/tls: conn closed")
	}
	if err := c.q.HandleData(toStdLevel(level), data); err != nil {
		return fmt.Errorf("quic/tls: HandleData: %w", err)
	}
	return nil
}

// Events drains all queued events from the underlying QUICConn.
func (c *Conn) Events() []Event {
	var out []Event
	for {
		ev := c.q.NextEvent()
		if ev.Kind == tls.QUICNoEvent {
			return out
		}
		switch ev.Kind {
		case tls.QUICSetReadSecret:
			out = append(out, Event{
				Kind:  EventSetReadSecret,
				Level: fromStdLevel(ev.Level),
				Suite: ev.Suite,
				Data:  append([]byte(nil), ev.Data...),
			})
		case tls.QUICSetWriteSecret:
			out = append(out, Event{
				Kind:  EventSetWriteSecret,
				Level: fromStdLevel(ev.Level),
				Suite: ev.Suite,
				Data:  append([]byte(nil), ev.Data...),
			})
		case tls.QUICWriteData:
			out = append(out, Event{
				Kind:  EventWriteData,
				Level: fromStdLevel(ev.Level),
				Data:  append([]byte(nil), ev.Data...),
			})
		case tls.QUICTransportParameters:
			out = append(out, Event{
				Kind: EventTransportParameters,
				Data: append([]byte(nil), ev.Data...),
			})
		case tls.QUICTransportParametersRequired:
			out = append(out, Event{Kind: EventTransportParametersRequired})
		case tls.QUICHandshakeDone:
			out = append(out, Event{Kind: EventHandshakeComplete})
		}
	}
}

// SendSessionTicket asks the TLS state machine to emit a
// NewSessionTicket post-handshake message. Must be called only after
// EventHandshakeComplete has been observed. The resulting bytes arrive
// via a subsequent QUICWriteData event at LevelApplication; the caller
// drains with Events() as usual and routes them into a CRYPTO frame
// inside a 1-RTT packet.
//
// EarlyData is hard-coded off. Enabling 0-RTT requires replay
// protection at the request layer (RFC 8470) which lives outside this
// driver.
func (c *Conn) SendSessionTicket() error {
	if c.closed {
		return errors.New("quic/tls: conn closed")
	}
	return c.q.SendSessionTicket(tls.QUICSessionTicketOptions{EarlyData: false})
}

// Close shuts the TLS state machine down.
func (c *Conn) Close() error {
	c.closed = true
	return c.q.Close()
}

// ConnectionState returns the stdlib TLS connection state — useful for
// ALPN inspection after the handshake completes.
func (c *Conn) ConnectionState() tls.ConnectionState {
	return c.q.ConnectionState()
}
