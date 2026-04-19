package loadbalance

import (
	"testing"
)

func TestP2CSingleAddr(t *testing.T) {
	p := NewP2C(1)
	for i := 0; i < 10; i++ {
		if got := p.Pick(1); got != 0 {
			t.Fatalf("Pick(1) = %d, want 0", got)
		}
	}
}

func TestP2CNeverPicksSameTwiceInSamplePair(t *testing.T) {
	// We can't directly observe the two samples, but we can confirm
	// that across many picks we see every index.
	p := NewP2C(4)
	seen := make(map[int]int)
	for i := 0; i < 1000; i++ {
		seen[p.Pick(4)]++
	}
	for i := 0; i < 4; i++ {
		if seen[i] == 0 {
			t.Fatalf("addr %d never picked across 1000 calls", i)
		}
	}
}

func TestP2CPrefersLowerEWMA(t *testing.T) {
	// Address 0 is slow (100ms), address 1 is fast (1ms). After enough
	// samples to saturate EWMA, Pick should lean heavily toward 1.
	p := NewP2C(2)
	for i := 0; i < 100; i++ {
		p.Update(0, 100_000_000)
		p.Update(1, 1_000_000)
	}
	hits := [2]int{}
	for i := 0; i < 2000; i++ {
		hits[p.Pick(2)]++
	}
	// With p2c over 2 addresses, picking two random samples always
	// yields one of each, so the faster wins every time.
	if hits[1] < hits[0] {
		t.Fatalf("p2c did not prefer faster addr: slow=%d fast=%d", hits[0], hits[1])
	}
	// The slow addr should essentially never win when EWMAs diverge
	// by 100x over 2 addrs.
	if hits[0] > 10 {
		t.Fatalf("slow addr won too often: %d/%d", hits[0], hits[1])
	}
}

func TestP2CZeroEWMAPreferred(t *testing.T) {
	// New address (EWMA = 0) should be preferred over a measured slow one.
	p := NewP2C(2)
	for i := 0; i < 10; i++ {
		p.Update(0, 50_000_000)
	}
	// Addr 1 has zero EWMA.
	hits := [2]int{}
	for i := 0; i < 1000; i++ {
		hits[p.Pick(2)]++
	}
	if hits[1] == 0 {
		t.Fatal("zero-EWMA addr should be preferred")
	}
}

func TestP2CEWMAConverges(t *testing.T) {
	// Feed steady 10ms samples; EWMA should stabilise near 10ms.
	p := NewP2C(1)
	sample := uint64(10_000_000)
	for i := 0; i < 200; i++ {
		p.Update(0, sample)
	}
	got := p.ewmaNs[0].Load()
	// Within 5% of sample.
	low := sample - sample/20
	high := sample + sample/20
	if got < low || got > high {
		t.Fatalf("EWMA did not converge: got %d want %d±5%%", got, sample)
	}
}

func TestP2CSkipsZeroLatencyUpdate(t *testing.T) {
	p := NewP2C(1)
	p.Update(0, 5_000_000)
	before := p.ewmaNs[0].Load()
	p.Update(0, 0) // no-op
	if p.ewmaNs[0].Load() != before {
		t.Fatal("zero latency mutated EWMA")
	}
}
