package loadbalance

import "sync/atomic"

// BudgetConfig configures the retry token bucket for one pool.
type BudgetConfig struct {
	// RetryPercent is the fraction of successful requests that replenish
	// one retry token. E.g. 20 means one token per 5 successes.
	// Clamped to [1, 100]; default 20.
	RetryPercent int
	// MinTokens is the floor guaranteed regardless of recent traffic.
	// Default 3. This ensures retries are always possible at startup or
	// after a long idle period even before successes have accrued.
	MinTokens int
}

// Budget is a token bucket that bounds retry traffic to a configurable
// percentage of successful requests. Call AllowRetry before every retry
// attempt; call RecordSuccess after every non-error response.
//
// All operations are atomic and safe for concurrent use.
type Budget struct {
	tokens  atomic.Int32 // current available tokens
	counter atomic.Int32 // successes since last token was added

	successPerToken int32 // = 100 / retryPercent
	maxTokens       int32
}

// NewBudget constructs a Budget from cfg, applying defaults for zero values.
func NewBudget(cfg BudgetConfig) *Budget {
	pct := cfg.RetryPercent
	if pct <= 0 {
		pct = 20
	}
	if pct > 100 {
		pct = 100
	}
	min := cfg.MinTokens
	if min <= 0 {
		min = 3
	}
	spt := int32(100 / pct)
	if spt <= 0 {
		spt = 1
	}
	b := &Budget{
		successPerToken: spt,
		maxTokens:       int32(min),
	}
	b.tokens.Store(int32(min))
	return b
}

// AllowRetry tries to consume one retry token. Returns true when a
// token was available and consumed; false when the budget is exhausted.
// Tokens are not returned on retry failure — each attempt pays one
// token regardless of outcome, so a sequence of failing retries drains
// the budget and stops the storm.
func (b *Budget) AllowRetry() bool {
	for {
		t := b.tokens.Load()
		if t <= 0 {
			return false
		}
		if b.tokens.CompareAndSwap(t, t-1) {
			return true
		}
	}
}

// RecordSuccess is called after each non-error upstream response
// (status < 500). It replenishes one token every successPerToken calls,
// capped at maxTokens, so the budget tracks a sliding window of recent
// traffic without growing unboundedly.
func (b *Budget) RecordSuccess() {
	for {
		c := b.counter.Load()
		nc := c + 1
		if nc >= b.successPerToken {
			// Reset counter and try to add a token.
			if b.counter.CompareAndSwap(c, 0) {
				for {
					t := b.tokens.Load()
					if t >= b.maxTokens {
						break
					}
					if b.tokens.CompareAndSwap(t, t+1) {
						break
					}
				}
				return
			}
			// CAS lost; retry the outer loop.
			continue
		}
		if b.counter.CompareAndSwap(c, nc) {
			return
		}
	}
}

// Available returns the current token count. Informational; the value
// may be stale by the time the caller acts on it.
func (b *Budget) Available() int { return int(b.tokens.Load()) }
