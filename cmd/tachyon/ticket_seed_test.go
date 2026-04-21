package main

import (
	"bytes"
	"os"
	"testing"
)

// TestEnsureTicketSeedGeneratesAndExports confirms the parent-side
// bootstrap both returns a seed AND writes it into os.Environ so that
// forked children inherit it. This is the contract ForkWorkers relies
// on — it copies os.Environ() into cmd.Env unchanged.
func TestEnsureTicketSeedGeneratesAndExports(t *testing.T) {
	t.Setenv(envTicketSeed, "")

	seed := ensureTicketSeed()
	if len(seed) != 32 {
		t.Fatalf("seed length = %d, want 32", len(seed))
	}

	// Child-side read path sees the same bytes.
	got := readTicketSeed()
	if !bytes.Equal(got, seed) {
		t.Fatalf("readTicketSeed != ensureTicketSeed:\n  ensure=%x\n  read  =%x", seed, got)
	}
}

// TestEnsureTicketSeedRespectsExistingEnv makes sure an operator-
// provided seed (systemd EnvironmentFile, k8s Secret) survives
// startup — we must NOT regenerate one on top.
func TestEnsureTicketSeedRespectsExistingEnv(t *testing.T) {
	want := bytes.Repeat([]byte{0x7F}, 32)
	t.Setenv(envTicketSeed, "7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f7f")

	got := ensureTicketSeed()
	if !bytes.Equal(got, want) {
		t.Fatalf("operator-provided seed was overwritten: got %x, want %x", got, want)
	}
}

// TestReadTicketSeedMalformed: a stale/corrupted env var must not
// crash the worker. Silent nil is the right fallback — the rotator
// drops to the per-process random path.
func TestReadTicketSeedMalformed(t *testing.T) {
	t.Setenv(envTicketSeed, "not-hex-at-all")
	if got := readTicketSeed(); got != nil {
		t.Errorf("readTicketSeed on malformed env = %x, want nil", got)
	}
	t.Setenv(envTicketSeed, "deadbeef") // valid hex, wrong length
	if got := readTicketSeed(); got != nil {
		t.Errorf("readTicketSeed on short env = %x, want nil", got)
	}
}

// Belt-and-suspenders: guard against a silent refactor that unsets
// the env var in ensureTicketSeed.
func TestEnsureTicketSeedEnvPersists(t *testing.T) {
	t.Setenv(envTicketSeed, "")
	_ = ensureTicketSeed()
	if os.Getenv(envTicketSeed) == "" {
		t.Fatal("envTicketSeed not exported after ensureTicketSeed()")
	}
}
