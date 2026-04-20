package congestion

import (
	"testing"
	"time"
)

func TestInitialWindow(t *testing.T) {
	n := New()
	if n.Window() != InitialWindowPackets*MaxDatagramSize {
		t.Fatalf("cwnd=%d", n.Window())
	}
	if !n.CanSend(0) {
		t.Fatal("should allow send at start")
	}
	if n.CanSend(n.Window()) {
		t.Fatal("should block at cwnd")
	}
}

func TestSlowStartDoubles(t *testing.T) {
	n := New()
	start := n.Cwnd
	now := time.Now()
	n.OnAck(MaxDatagramSize, now, now)
	if n.Cwnd != start+MaxDatagramSize {
		t.Fatalf("cwnd=%d want %d", n.Cwnd, start+MaxDatagramSize)
	}
}

func TestLossHalves(t *testing.T) {
	n := New()
	now := time.Now()
	before := n.Cwnd
	n.OnLost(MaxDatagramSize, now)
	if n.Cwnd != before/2 {
		t.Fatalf("cwnd=%d want %d", n.Cwnd, before/2)
	}
	if n.Ssthresh != n.Cwnd {
		t.Fatalf("ssthresh=%d cwnd=%d", n.Ssthresh, n.Cwnd)
	}
}

func TestRecoveryPeriodSwallowsSecondLoss(t *testing.T) {
	n := New()
	t0 := time.Now()
	n.OnLost(MaxDatagramSize, t0.Add(10*time.Millisecond))
	after := n.Cwnd
	// A second loss for an older packet should not halve again.
	n.OnLost(MaxDatagramSize, t0.Add(5*time.Millisecond))
	if n.Cwnd != after {
		t.Fatalf("cwnd changed on in-recovery loss: %d vs %d", n.Cwnd, after)
	}
	// A loss newer than the recovery start should.
	n.OnLost(MaxDatagramSize, t0.Add(20*time.Millisecond))
	if n.Cwnd >= after {
		t.Fatalf("expected halving on newer loss, cwnd=%d", n.Cwnd)
	}
}

func TestCongestionAvoidance(t *testing.T) {
	n := New()
	n.Ssthresh = n.Cwnd // force CA on next ack
	before := n.Cwnd
	now := time.Now()
	n.OnAck(MaxDatagramSize, now, now)
	// Expected growth: MSS*MSS/cwnd = 1200/10 = 120.
	if n.Cwnd-before <= 0 || n.Cwnd-before > MaxDatagramSize {
		t.Fatalf("CA growth off: %d → %d", before, n.Cwnd)
	}
}

func TestPersistentCongestion(t *testing.T) {
	n := New()
	n.OnPersistentCong()
	if n.Cwnd != PersistentCongWindow {
		t.Fatalf("cwnd=%d", n.Cwnd)
	}
}

func TestMinWindowFloor(t *testing.T) {
	n := New()
	now := time.Now()
	for i := 0; i < 20; i++ {
		n.OnLost(MaxDatagramSize, now.Add(time.Duration(i)*time.Second))
	}
	if n.Cwnd < MinWindowPackets*MaxDatagramSize {
		t.Fatalf("cwnd under floor: %d", n.Cwnd)
	}
}
