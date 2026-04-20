package recovery

import (
	"testing"
	"time"
)

func mkPkt(n uint64, sp Space, t time.Time, ack bool) Packet {
	return Packet{Number: n, Space: sp, SentTime: t, Size: 1200, AckEliciting: ack, InFlight: true}
}

func TestFirstSampleSeeds(t *testing.T) {
	r := New(25 * time.Millisecond)
	t0 := time.Now()
	r.OnSent(mkPkt(0, SpaceApplication, t0, true))
	got := r.OnAck(SpaceApplication, 0, [][2]uint64{{0, 0}}, 0, t0.Add(100*time.Millisecond))
	if len(got) != 1 {
		t.Fatalf("want 1 newly-acked, got %d", len(got))
	}
	if r.SRTT != 100*time.Millisecond {
		t.Fatalf("SRTT=%v", r.SRTT)
	}
	if r.MinRTT != 100*time.Millisecond {
		t.Fatalf("MinRTT=%v", r.MinRTT)
	}
	if r.RTTVar != 50*time.Millisecond {
		t.Fatalf("RTTVar=%v", r.RTTVar)
	}
}

func TestAckDelayClipped(t *testing.T) {
	r := New(25 * time.Millisecond)
	t0 := time.Now()
	// Seed first sample.
	r.OnSent(mkPkt(0, SpaceApplication, t0, true))
	r.OnAck(SpaceApplication, 0, [][2]uint64{{0, 0}}, 0, t0.Add(100*time.Millisecond))

	// Second sample with ack_delay > MaxAckDelay — clipped to 25ms.
	r.OnSent(mkPkt(1, SpaceApplication, t0.Add(200*time.Millisecond), true))
	r.OnAck(SpaceApplication, 1, [][2]uint64{{1, 1}}, 500*time.Millisecond, t0.Add(400*time.Millisecond))
	// adj = 200ms - 25ms = 175ms >= MinRTT; updated SRTT = (7*100 + 175)/8 = 109.375ms.
	want := (7*100*time.Millisecond + 175*time.Millisecond) / 8
	if r.SRTT != want {
		t.Fatalf("SRTT=%v want %v", r.SRTT, want)
	}
}

func TestNonElicitingLargestDoesNotUpdate(t *testing.T) {
	r := New(0)
	t0 := time.Now()
	r.OnSent(mkPkt(5, SpaceApplication, t0, false))
	r.OnAck(SpaceApplication, 5, [][2]uint64{{5, 5}}, 0, t0.Add(50*time.Millisecond))
	if r.sampled {
		t.Fatal("RTT should not be sampled from non-ack-eliciting")
	}
}

func TestLossPacketThreshold(t *testing.T) {
	r := New(0)
	t0 := time.Now()
	for i := uint64(0); i < 5; i++ {
		r.OnSent(mkPkt(i, SpaceApplication, t0.Add(time.Duration(i)*time.Millisecond), true))
	}
	// Ack #4 only. #0 and #1 are >=3 behind and should be declared lost.
	r.OnAck(SpaceApplication, 4, [][2]uint64{{4, 4}}, 0, t0.Add(10*time.Millisecond))
	lost := r.DetectLoss(SpaceApplication, t0.Add(20*time.Millisecond))
	if len(lost) < 2 {
		t.Fatalf("expected >=2 lost, got %d", len(lost))
	}
}

func TestLossTimeThreshold(t *testing.T) {
	r := New(0)
	t0 := time.Now()
	// Establish an RTT.
	r.OnSent(mkPkt(0, SpaceApplication, t0, true))
	r.OnAck(SpaceApplication, 0, [][2]uint64{{0, 0}}, 0, t0.Add(10*time.Millisecond))
	// Send 1 and 2 close together, ack only 2.
	r.OnSent(mkPkt(1, SpaceApplication, t0.Add(20*time.Millisecond), true))
	r.OnSent(mkPkt(2, SpaceApplication, t0.Add(21*time.Millisecond), true))
	r.OnAck(SpaceApplication, 2, [][2]uint64{{2, 2}}, 0, t0.Add(30*time.Millisecond))
	// Far-future detection: #1's send time is well past lossDelay.
	lost := r.DetectLoss(SpaceApplication, t0.Add(1*time.Second))
	found := false
	for _, p := range lost {
		if p.Number == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected packet 1 lost by time threshold, got %d packets", len(lost))
	}
}

func TestPTOBackoff(t *testing.T) {
	r := New(25 * time.Millisecond)
	base := r.PTOPeriod()
	r.OnPTO()
	if r.PTOPeriod() != 2*base {
		t.Fatalf("want 2x after one PTO, got %v vs %v", r.PTOPeriod(), base)
	}
	r.OnPTO()
	if r.PTOPeriod() != 4*base {
		t.Fatalf("want 4x after two PTOs, got %v", r.PTOPeriod())
	}
}

func TestAckResetsPTOCount(t *testing.T) {
	r := New(0)
	r.OnPTO()
	r.OnPTO()
	t0 := time.Now()
	r.OnSent(mkPkt(0, SpaceApplication, t0, true))
	r.OnAck(SpaceApplication, 0, [][2]uint64{{0, 0}}, 0, t0.Add(10*time.Millisecond))
	if r.PTOCount != 0 {
		t.Fatalf("PTOCount=%d after ack", r.PTOCount)
	}
}
