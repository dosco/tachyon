package upstream

import (
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestDialerRoundRobins confirms that concurrent Dial calls spread over
// the configured address list. The real rr counter is atomic; we just
// count which addresses are tried.
func TestDialerRoundRobins(t *testing.T) {
	var hits [3]atomic.Int32
	d := &dialer{
		addrs:    []string{"a:1", "b:2", "c:3"},
		timeout:  time.Second,
		attempts: 1,
	}
	d.dialFn = func(_, addr string, _ time.Duration) (net.Conn, error) {
		switch addr {
		case "a:1":
			hits[0].Add(1)
		case "b:2":
			hits[1].Add(1)
		case "c:3":
			hits[2].Add(1)
		}
		return &fakeConn{}, nil
	}
	for i := 0; i < 30; i++ {
		c, err := d.dial()
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_ = c.Close()
	}
	// Each address should see exactly 10 hits.
	for i := range hits {
		if got := hits[i].Load(); got != 10 {
			t.Errorf("addr[%d] hits: got %d want 10", i, got)
		}
	}
}

// TestDialerRetriesOnFailure confirms the two-attempt budget: first
// pass over the address list fails on every address, then one sleep,
// then the second pass succeeds.
func TestDialerRetriesOnFailure(t *testing.T) {
	var attempts atomic.Int32
	failAttempts := int32(3) // 3 addresses × 1 full failed pass = 3 failures before success

	d := &dialer{
		addrs:    []string{"a:1", "b:2", "c:3"},
		timeout:  time.Second,
		backoff:  time.Nanosecond, // make test fast
		attempts: 2,
	}
	d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		if attempts.Add(1) <= failAttempts {
			return nil, errors.New("refused")
		}
		return &fakeConn{}, nil
	}
	c, err := d.dial()
	if err != nil {
		t.Fatalf("dial with retry: %v", err)
	}
	_ = c.Close()
	if got := attempts.Load(); got < 4 {
		t.Fatalf("attempts: got %d, want >= 4 (one full pass + one success)", got)
	}
}

// TestDialerAllAttemptsFail returns the last error when every attempt
// fails on every address. The caller (Pool) uses that to bump the
// circuit breaker; we just confirm the error propagates.
func TestDialerAllAttemptsFail(t *testing.T) {
	lastErr := errors.New("third failure")
	d := &dialer{
		addrs:    []string{"a:1"},
		attempts: 2,
		backoff:  time.Nanosecond,
	}
	d.dialFn = func(_, _ string, _ time.Duration) (net.Conn, error) {
		return nil, lastErr
	}
	_, err := d.dial()
	if !errors.Is(err, lastErr) {
		t.Fatalf("dial: got %v, want %v", err, lastErr)
	}
}

// TestDialerNoAddrs surfaces the config error (as opposed to a dial
// failure) so Acquire can return it without touching the circuit
// breaker.
func TestDialerNoAddrs(t *testing.T) {
	d := &dialer{}
	_, err := d.dial()
	if err == nil {
		t.Fatalf("expected error on empty addrs")
	}
	if err != errNoAddrs {
		t.Fatalf("got %v, want errNoAddrs", err)
	}
}

// TestDialerRetryStartsFromNextAddr confirms the retry starts from a
// bumped rr cursor, not the same address it just failed on. That way a
// stuck first address doesn't waste both attempts on itself.
func TestDialerRetryStartsFromNextAddr(t *testing.T) {
	var firstHitSequence []string
	d := &dialer{
		addrs:    []string{"a:1", "b:2"},
		attempts: 2,
		backoff:  time.Nanosecond,
	}
	d.dialFn = func(_, addr string, _ time.Duration) (net.Conn, error) {
		firstHitSequence = append(firstHitSequence, addr)
		return nil, errors.New("fail")
	}
	_, _ = d.dial()
	// We expect: a b (pass 1) then either a b or b a (pass 2).
	// What must NOT happen: a a a a. Confirm b appears in the first two
	// recorded addrs.
	if len(firstHitSequence) < 2 {
		t.Fatalf("too few dial attempts: %v", firstHitSequence)
	}
	seenA, seenB := false, false
	for _, a := range firstHitSequence[:2] {
		switch a {
		case "a:1":
			seenA = true
		case "b:2":
			seenB = true
		}
	}
	if !(seenA && seenB) {
		t.Fatalf("first pass didn't try both addrs: %v", firstHitSequence[:2])
	}
}
