package upstream

// HotPool is a lock-free variant of Pool designed for the GOMAXPROCS=1
// worker-per-core model. Because exactly one goroutine touches it, the
// mutex in Pool would be uncontended overhead; HotPool deletes it entirely.
//
// In Phase 0 we wire Pool everywhere. HotPool is kept here so the later
// phases have a drop-in replacement with the same shape.
type HotPool struct {
	d       *dialer
	maxIdle int
	idle    []*Conn
}

// NewHotPool mirrors newPool.
func NewHotPool(addrs []string, timeoutSeconds int, maxIdle int) *HotPool {
	_ = timeoutSeconds // wired in Phase 2 when we connect via io_uring
	return &HotPool{
		d:       &dialer{addrs: addrs},
		maxIdle: maxIdle,
		idle:    make([]*Conn, 0, maxIdle),
	}
}

func (p *HotPool) Acquire() (*Conn, error) {
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		return c, nil
	}
	return p.d.dial()
}

func (p *HotPool) Release(c *Conn) {
	if c == nil || c.IsBroken() {
		if c != nil {
			_ = c.Close()
		}
		return
	}
	if len(p.idle) >= p.maxIdle {
		_ = c.Close()
		return
	}
	p.idle = append(p.idle, c)
}
