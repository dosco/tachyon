//go:build !linux

package runtime

import "errors"

const (
	EnvWorkerID    = "TACHYON_WORKER_ID"
	EnvWorkerCount = "TACHYON_WORKER_COUNT"
)

// ForkWorkers is only implemented on Linux. On other platforms the main
// program runs inline and this returns an error if called.
func ForkWorkers(n int) error {
	return errors.New("runtime: worker fork requires linux; run with -workers=1")
}
