//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// EnvWorkerID is set on child processes so they skip the fork step and just
// serve. The value is the 0-based worker index, used to compute CPU affinity.
const EnvWorkerID = "TACHYON_WORKER_ID"

// EnvWorkerCount is the total worker count, for logging.
const EnvWorkerCount = "TACHYON_WORKER_COUNT"

// CanFork reports whether multi-process worker forking is supported on this
// platform. It is true on Linux and false everywhere else.
func CanFork() bool { return true }

// ForkWorkers re-execs the current binary n times with EnvWorkerID set on
// each child. Returns after all children have exited.
func ForkWorkers(n int) error {
	children := make([]*exec.Cmd, 0, n)
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("forkWorkers: executable: %w", err)
	}
	for i := 0; i < n; i++ {
		cmd := exec.Command(self, os.Args[1:]...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(),
			EnvWorkerID+"="+strconv.Itoa(i),
			EnvWorkerCount+"="+strconv.Itoa(n),
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			// Die with the parent if the parent crashes.
			Pdeathsig: syscall.SIGTERM,
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("forkWorkers: start %d: %w", i, err)
		}
		children = append(children, cmd)
	}
	// Wait for all.
	var firstErr error
	for _, c := range children {
		if err := c.Wait(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
