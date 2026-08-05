//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// terminateOrphanProcessGroup sends SIGTERM then SIGKILL to the entire
// process group led by pid. Everything we record is spawned with Setpgid, so
// the leader's pgid equals its pid, and killing the group reaps whatever it
// spawned: helper children of a managed server (language servers, sub-shells)
// and, for a native agent leader, the test runners a Test-step agent started.
//
// expectedStart is the process start time the PID record was written with, and
// it is re-proven here rather than trusted from the caller. Two things make
// that necessary. The caller validates identity before any signal, but this
// function then waits up to three seconds for a graceful exit, and a PID freed
// during that wait can be reused by an unrelated process before SIGKILL lands.
// And the group is addressed by pgid, so a misidentified target costs not one
// wrong process but a whole wrong group - potentially the daemon's own.
// A zero expectedStart means the record predates start-time tracking and there
// is nothing to re-prove against.
func terminateOrphanProcessGroup(pid int, expectedStart time.Time) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Exited between the caller's check and now: nothing to kill.
			return nil
		}
		return fmt.Errorf("getpgid %d: %w", pid, err)
	}
	// Never guess the group. Every leader we record is its own group leader; a
	// pid that is not means either the record does not describe what we spawned
	// or the pid has been reused by a process belonging to someone else's group
	// - possibly ours. Signalling -pgid there would kill that whole group.
	if pgid != pid {
		return fmt.Errorf("pid %d is not its own process-group leader (pgid %d); refusing to signal an unrelated group", pid, pgid)
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sigterm pgid %d: %w", pgid, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := processRunningFunc(pid); !alive {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !expectedStart.IsZero() {
		matches, err := startTimeMatches(pid, expectedStart)
		if err != nil {
			return fmt.Errorf("revalidate pid %d before sigkill: %w", pid, err)
		}
		if !matches {
			return fmt.Errorf("pid %d was reused during termination; not sending sigkill", pid)
		}
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("sigkill pgid %d: %w", pgid, err)
	}
	return nil
}
