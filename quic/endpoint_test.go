package quic

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"tachyon/quic/packet"
)

// buildInitialV1 mirrors the helper in quic/packet/header_test.go but
// lives here to avoid a test-only exported API.
func buildInitialV1(version uint32, dcid, scid []byte) []byte {
	b := []byte{0xc0}
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v, version)
	b = append(b, v...)
	b = append(b, byte(len(dcid)))
	b = append(b, dcid...)
	b = append(b, byte(len(scid)))
	b = append(b, scid...)
	b = packet.AppendVarint(b, 0) // token length
	b = packet.AppendVarint(b, 4) // length
	b = append(b, 0, 0, 0, 0)
	return b
}

func newLoopbackEndpoint(t *testing.T) (*Endpoint, net.Addr) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	ep := NewEndpoint(pc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return ep, pc.LocalAddr()
}

func TestEndpointAcceptsInitial(t *testing.T) {
	ep, srvAddr := newLoopbackEndpoint(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ctx) }()

	cli, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client ListenPacket: %v", err)
	}
	defer cli.Close()

	dcid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	scid := []byte{9, 10, 11, 12}
	pkt := buildInitialV1(packet.Version1, dcid, scid)
	if _, err := cli.WriteTo(pkt, srvAddr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	// Wait briefly for the server loop to process the packet.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ep.Stats().PacketsIn >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ep.Stats().PacketsIn == 0 {
		t.Fatalf("no packets received by endpoint")
	}
	// Phase 2: without a TLS config the endpoint drops incoming Initials.
	// The structural-parse path is still exercised — we just want to see
	// that the packet was counted in.

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestEndpointVersionNegotiation(t *testing.T) {
	ep, srvAddr := newLoopbackEndpoint(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ctx) }()

	cli, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client ListenPacket: %v", err)
	}
	defer cli.Close()

	dcid := []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11}
	scid := []byte{1, 2, 3, 4}
	// Unknown version — server must reply with VN.
	pkt := buildInitialV1(0xdeadbeef, dcid, scid)
	if _, err := cli.WriteTo(pkt, srvAddr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	_ = cli.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := cli.ReadFrom(buf)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	h, perr := packet.Parse(buf[:n], 0)
	if perr == nil || perr.Error() != packet.ErrVersionNeg.Error() {
		t.Fatalf("reply not VN: err=%v header=%+v", perr, h)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
