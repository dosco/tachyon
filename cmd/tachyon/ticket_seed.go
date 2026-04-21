package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
)

// envTicketSeed is the environment variable that carries the shared
// TLS session-ticket seed from the parent process to every
// SO_REUSEPORT worker. Operators can pre-set it (via systemd
// EnvironmentFile, k8s Secret, etc.) to keep ticket continuity across
// rolling restarts; otherwise the parent generates a random 32-byte
// seed on startup and exports it to its own os.Environ so the
// workers inherit it.
//
// 32 bytes, hex-encoded (64 ASCII chars).
const envTicketSeed = "TACHYON_TLS_TICKET_SEED"

// ensureTicketSeed returns a 32-byte seed for TLS session-ticket
// derivation. If envTicketSeed is present and correctly sized it is
// decoded and returned unchanged; otherwise a fresh random seed is
// generated and written back into the process environment so
// subsequently-forked children inherit it verbatim.
//
// Returns nil only on unrecoverable rand read failure, which we treat
// as fatal at the call site.
func ensureTicketSeed() []byte {
	if v := os.Getenv(envTicketSeed); v != "" {
		if b, err := hex.DecodeString(v); err == nil && len(b) == 32 {
			return b
		}
		// Malformed: ignore and regenerate. Prefer silent recovery
		// over failing startup on a stale env var.
	}
	var buf [32]byte
	if _, err := io.ReadFull(rand.Reader, buf[:]); err != nil {
		return nil
	}
	_ = os.Setenv(envTicketSeed, hex.EncodeToString(buf[:]))
	return buf[:]
}

// readTicketSeed returns the seed from the environment, or nil if
// unset or malformed. Workers call this; they never generate a seed
// of their own, because a worker-generated seed defeats the whole
// point (every worker would pick a different one).
func readTicketSeed() []byte {
	v := os.Getenv(envTicketSeed)
	if v == "" {
		return nil
	}
	b, err := hex.DecodeString(v)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}
