//go:build !unix && !windows

package shellenv

import (
	"errors"
	"os/exec"
)

// ConfigureShellCommand is a no-op on platforms that lack process groups
// (and a process-tree kill primitive). Context cancellation falls back to the
// exec.CommandContext default of terminating the direct child only.
func ConfigureShellCommand(cmd *exec.Cmd) {}

// StartShellCommand starts cmd on platforms without extra process-tree setup.
// It exists so call sites can use the same lifecycle helpers on every platform.
func StartShellCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// TerminateShellCommandGroup cannot reap a process tree on platforms without a
// process-tree kill primitive, mirroring ConfigureShellCommand. The
// reap-the-group-on-exit guarantee is best-effort and platform-gated. It
// reports an error for a started command rather than claiming success, so a
// caller holding a crash-recovery record keeps that record instead of dropping
// it on the strength of a kill that never happened.
func TerminateShellCommandGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return errors.New("process-group termination is unsupported on this platform")
}
