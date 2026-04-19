//go:build !linux

package runtime

import "errors"

const (
	EnvWorkerID    = "TACHYON_WORKER_ID"
	EnvWorkerCount = "TACHYON_WORKER_COUNT"
)

// CanFork reports whether multi-process worker forking is supported.
// Always false on non-Linux; tachyon runs as a single process.
func CanFork() bool { return false }

// ForkWorkers is not implemented on non-Linux. Never called because
// CanFork() returns false.
func ForkWorkers(n int) error {
	return errors.New("runtime: worker fork requires linux")
}
