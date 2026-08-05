//go:build windows

package daemon

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

var taskkillProcessTree = func(pid int) ([]byte, error) {
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	winproc.Harden(cmd)
	return cmd.CombinedOutput()
}

// terminateOrphanProcessGroup forcibly terminates the orphaned process tree.
//
// expectedStart is accepted for parity with the Unix implementation but is not
// re-checked: taskkill /T /F is a single immediate call, so unlike the Unix
// SIGTERM-then-SIGKILL sequence there is no wait between signals during which
// the PID could be released and reused. The caller's pre-signal validation is
// the only window, and it is the same one either platform has.
func terminateOrphanProcessGroup(pid int, _ time.Time) error {
	out, err := taskkillProcessTree(pid)
	if err != nil {
		alive, runningErr := processRunningFunc(pid)
		if runningErr == nil && !alive {
			return nil
		}
		output := strings.TrimSpace(string(out))
		if output != "" {
			return fmt.Errorf("taskkill pid %d: %w: %s", pid, err, output)
		}
		return fmt.Errorf("taskkill pid %d: %w", pid, err)
	}
	return nil
}
