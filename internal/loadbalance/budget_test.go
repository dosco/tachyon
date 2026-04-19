package loadbalance

import "testing"

// TestBudgetInitialTokens confirms that a fresh budget has minTokens
// available before any successes have accrued.
func TestBudgetInitialTokens(t *testing.T) {
	b := NewBudget(BudgetConfig{RetryPercent: 20, MinTokens: 3})
	if got := b.Available(); got != 3 {
		t.Fatalf("initial tokens: got %d want 3", got)
	}
}

// TestBudgetAllowRetryDrains confirms consecutive AllowRetry calls
// drain the token pool to zero.
func TestBudgetAllowRetryDrains(t *testing.T) {
	b := NewBudget(BudgetConfig{RetryPercent: 20, MinTokens: 3})
	for i := 0; i < 3; i++ {
		if !b.AllowRetry() {
			t.Fatalf("iter %d: expected token available", i)
		}
	}
	if b.AllowRetry() {
		t.Fatal("expected exhausted after 3 retries")
	}
}

// TestBudgetReplenishesAfterSuccesses confirms tokens are added at the
// configured rate (1 per successPerToken successes) and the cap is
// respected.
func TestBudgetReplenishesAfterSuccesses(t *testing.T) {
	// 50% → 1 token per 2 successes; cap at 2.
	b := NewBudget(BudgetConfig{RetryPercent: 50, MinTokens: 2})
	// Drain the initial tokens.
	b.AllowRetry()
	b.AllowRetry()
	if b.Available() != 0 {
		t.Fatalf("expected 0 after drain, got %d", b.Available())
	}
	// One success — not enough to add a token yet.
	b.RecordSuccess()
	if b.Available() != 0 {
		t.Fatalf("after 1 success: expected 0, got %d", b.Available())
	}
	// Second success — should add one token.
	b.RecordSuccess()
	if b.Available() != 1 {
		t.Fatalf("after 2 successes: expected 1, got %d", b.Available())
	}
	// More successes; tokens should not exceed cap (2).
	for i := 0; i < 100; i++ {
		b.RecordSuccess()
	}
	if got := b.Available(); got > 2 {
		t.Fatalf("tokens exceeded cap: got %d", got)
	}
}

// TestBudgetExhaustionBoundsRetries simulates a burst of failures
// after some successes and confirms the token pool bottoms out,
// demonstrating that the budget bounds the retry rate.
func TestBudgetExhaustionBoundsRetries(t *testing.T) {
	// 10% → 1 token per 10 successes; cap at 5.
	b := NewBudget(BudgetConfig{RetryPercent: 10, MinTokens: 5})

	allowed := 0
	// Mix successes and retry attempts at 5:1 success-to-retry ratio.
	// With a budget of 10%, we should allow far fewer than all retries.
	for i := 0; i < 1000; i++ {
		if b.AllowRetry() {
			allowed++
		}
		b.RecordSuccess()
	}
	// Budget allows roughly 10% of 1000 = ~100 retries, bounded by cap 5
	// at any given moment. We check a loose bound to avoid test flakiness.
	if allowed > 200 {
		t.Fatalf("too many retries allowed (%d); budget should limit to ~10%%", allowed)
	}
}

// TestBudgetDefaultsApplied confirms that zero-value BudgetConfig
// results in a usable budget with the documented defaults.
func TestBudgetDefaultsApplied(t *testing.T) {
	b := NewBudget(BudgetConfig{})
	// Default MinTokens = 3.
	if got := b.Available(); got != 3 {
		t.Fatalf("default initial tokens: got %d want 3", got)
	}
	// Default RetryPercent = 20 → 5 successes per token.
	b.AllowRetry()
	b.AllowRetry()
	b.AllowRetry()
	// Should be exhausted.
	if b.AllowRetry() {
		t.Fatal("budget not exhausted after default-min retries")
	}
	// 5 successes should restore one token.
	for i := 0; i < 5; i++ {
		b.RecordSuccess()
	}
	if got := b.Available(); got != 1 {
		t.Fatalf("after 5 successes: expected 1 token, got %d", got)
	}
}
