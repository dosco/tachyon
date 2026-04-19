package buf

import "sync"

// One sync.Pool per class. sync.Pool is internally sharded per-P, so with our
// GOMAXPROCS=1 workers Get/Put is effectively a single linked-list pop.
var pools [numClasses]sync.Pool

func init() {
	for c := Class(0); c < numClasses; c++ {
		size := c.Size()
		class := c // capture
		pools[c].New = func() any {
			return &Slab{class: class, b: make([]byte, size)}
		}
	}
}

// Get returns a Slab of the requested class. The returned buffer's contents
// are undefined; callers must write before reading.
func Get(c Class) *Slab {
	return pools[c].Get().(*Slab)
}

// Put returns a Slab to its class pool. Before returning, the slab's
// written region is zeroed so the next borrower cannot observe stale
// bytes from this request. After Put the caller must not touch the
// slab again.
func Put(s *Slab) {
	if s == nil {
		return
	}
	s.Reset()
	pools[s.class].Put(s)
}
