package loadbalance

import (
	"sync"
	"testing"
)

func TestRRSpreadsEvenly(t *testing.T) {
	r := NewRR()
	const n = 4
	const rounds = 1000
	counts := make([]int, n)
	for i := 0; i < n*rounds; i++ {
		counts[r.Pick(n)]++
	}
	for i, c := range counts {
		if c != rounds {
			t.Errorf("addr[%d] hits: got %d want %d", i, c, rounds)
		}
	}
}

func TestRRConcurrentCoverage(t *testing.T) {
	r := NewRR()
	const n = 3
	const perG = 300
	const goroutines = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := make([]int, n)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]int, n)
			for i := 0; i < perG; i++ {
				local[r.Pick(n)]++
			}
			mu.Lock()
			for i := range counts {
				counts[i] += local[i]
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	total := perG * goroutines
	expect := total / n
	for i, c := range counts {
		if c < expect-goroutines || c > expect+goroutines {
			t.Errorf("addr[%d] got %d, want ~%d (±%d)", i, c, expect, goroutines)
		}
	}
}
