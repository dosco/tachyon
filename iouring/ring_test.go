// End-to-end smoke tests for the io_uring binding. Verifies that:
//   - A Ring can be created and torn down,
//   - An OP_NOP round-trips through submit+drain,
//   - Multishot accept + provided-buffer recv produces CQEs on a
//     loopback connection.

//go:build linux

package iouring_test

import (
	"net"
	"syscall"
	"testing"
	"time"

	"tachyon/iouring"
	"tachyon/iouring/buffers"
	"tachyon/iouring/op"
)

func TestRingSetupAndNop(t *testing.T) {
	r, err := iouring.New(64, iouring.SetupClamp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	sqe, err := r.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	sqe.Opcode = iouring.OpNop
	sqe.UserData = 0xdead

	if _, err := r.SubmitAndWait(1); err != nil {
		t.Fatalf("SubmitAndWait: %v", err)
	}

	var got uint64
	r.Drain(func(c *iouring.CQE) bool {
		got = c.UserData
		return false
	})
	if got != 0xdead {
		t.Fatalf("userdata: got %x want dead", got)
	}
}

func TestProvidedBufferRing(t *testing.T) {
	r, err := iouring.New(64, iouring.SetupClamp)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	pbr, err := buffers.Provide(r.FD(), 1, 16, 4096)
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	defer pbr.Close()

	// Listen + connect on loopback.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("hello iouring"))
		time.Sleep(50 * time.Millisecond)
	}()

	tcp, err := ln.(*net.TCPListener).AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	rawConn, err := tcp.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd int
	rawConn.Control(func(f uintptr) { fd = int(f) })
	// Un-set O_NONBLOCK so the recv actually waits in the kernel.
	syscall.SetNonblock(fd, false)

	sqe, _ := r.Reserve()
	op.RecvMultishot(sqe, fd, pbr.GroupID(), 0x42)

	if _, err := r.SubmitAndWait(1); err != nil {
		t.Fatalf("SubmitAndWait: %v", err)
	}

	var n int
	var payload string
	r.Drain(func(c *iouring.CQE) bool {
		if c.Res < 0 {
			t.Fatalf("recv CQE err: %d", c.Res)
		}
		if c.Flags&iouring.CQEFBuffer == 0 {
			t.Fatalf("recv CQE did not use provided buffer (flags=%x)", c.Flags)
		}
		bid := iouring.BufferID(c)
		n = int(c.Res)
		payload = string(pbr.Bytes(bid, n))
		pbr.Recycle(bid)
		return false
	})
	if payload != "hello iouring" {
		t.Fatalf("recv: got %q (%d bytes)", payload, n)
	}
}
