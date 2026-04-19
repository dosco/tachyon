package loadbalance

import (
	"testing"
	"time"
)

func TestDetectorNilSafe(t *testing.T) {
	var d *Detector
	d.Record(0, 500, false) // must not panic
	if d.Ejected(0) {
		t.Fatal("nil detector reports ejected")
	}
}

func TestDetectorEjectsOnConsecutive5xx(t *testing.T) {
	d := NewDetector(3, OutlierConfig{
		Consecutive5xx:    3,
		EjectionDuration:  100 * time.Millisecond,
		MaxEjectedPercent: 100,
	})
	for i := 0; i < 3; i++ {
		d.Record(1, 503, false)
	}
	if !d.Ejected(1) {
		t.Fatal("addr 1 should be ejected after 3 consecutive 5xx")
	}
	if d.Ejected(0) || d.Ejected(2) {
		t.Fatal("other addrs should not be ejected")
	}
}

func TestDetectorEjectsOnConsecutiveGatewayErr(t *testing.T) {
	d := NewDetector(3, OutlierConfig{
		ConsecutiveGatewayErr: 2,
		EjectionDuration:      100 * time.Millisecond,
		MaxEjectedPercent:     100,
	})
	d.Record(2, 0, true)
	d.Record(2, 0, true)
	if !d.Ejected(2) {
		t.Fatal("addr 2 should be ejected after 2 consecutive gateway errors")
	}
}

func TestDetectorClearsStreakOnSuccess(t *testing.T) {
	d := NewDetector(3, OutlierConfig{
		Consecutive5xx:    3,
		EjectionDuration:  time.Second,
		MaxEjectedPercent: 100,
	})
	d.Record(0, 503, false)
	d.Record(0, 503, false)
	d.Record(0, 200, false) // success resets
	d.Record(0, 503, false)
	d.Record(0, 503, false)
	if d.Ejected(0) {
		t.Fatal("2+success+2 should not eject (streak reset)")
	}
}

func TestDetectorUnejectAfterDuration(t *testing.T) {
	d := NewDetector(3, OutlierConfig{
		Consecutive5xx:    1,
		EjectionDuration:  10 * time.Millisecond,
		MaxEjectedPercent: 100,
	})
	d.Record(0, 500, false)
	if !d.Ejected(0) {
		t.Fatal("should be ejected immediately")
	}
	time.Sleep(15 * time.Millisecond)
	if d.Ejected(0) {
		t.Fatal("should un-eject after duration")
	}
}

func TestDetectorRespectsMaxEjectedPercent(t *testing.T) {
	// 4 addrs, 50% cap → at most 2 ejected simultaneously.
	d := NewDetector(4, OutlierConfig{
		Consecutive5xx:    1,
		EjectionDuration:  time.Second,
		MaxEjectedPercent: 50,
	})
	d.Record(0, 500, false)
	d.Record(1, 500, false)
	d.Record(2, 500, false) // should be skipped
	ejected := 0
	for i := 0; i < 4; i++ {
		if d.Ejected(i) {
			ejected++
		}
	}
	if ejected != 2 {
		t.Fatalf("got %d ejected, want 2 (50%% cap of 4)", ejected)
	}
}

func TestDetectorGatewayAnd5xxAreSeparateStreaks(t *testing.T) {
	d := NewDetector(2, OutlierConfig{
		Consecutive5xx:        3,
		ConsecutiveGatewayErr: 3,
		EjectionDuration:      time.Second,
		MaxEjectedPercent:     100,
	})
	// Alternating: each record clears the other streak.
	d.Record(0, 500, false)
	d.Record(0, 0, true)
	d.Record(0, 500, false)
	d.Record(0, 0, true)
	if d.Ejected(0) {
		t.Fatal("interleaved errors should not hit either threshold")
	}
}
