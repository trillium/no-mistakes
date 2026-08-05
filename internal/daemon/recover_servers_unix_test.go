//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// spawnSleepProcess launches `sleep 30` in its own process group so that a
// reap test that kills the pgroup can't take out the go test runner. The
// returned *exec.Cmd lets the caller clean up with killAndWait.
func spawnSleepProcess(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	cmd := exec.Command(bin, "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	return cmd, cmd.Process.Pid
}

func killAndWait(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func waitForPIDExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive, _ := processRunning(pid)
		if !alive {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestReapOrphanedServers_KillsLiveMatchingProcess proves the full reap
// flow: a PID file whose recorded StartedAt matches the subprocess's real
// start time triggers a kill, and the PID file is removed.
func TestReapOrphanedServers_KillsLiveMatchingProcess(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	started, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("read start time: %v", err)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-live.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: started,
	})

	reapOrphanedServers(p)

	if !waitForPIDExit(pid, 5*time.Second) {
		t.Errorf("expected pid %d to be terminated", pid)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pid file should be removed after reap, got err=%v", err)
	}
}

// TestReapOrphanedServers_SkipsWhenDaemonAlive verifies that if the daemon
// PID file points at a running process, reaping is skipped entirely -
// protects a concurrently-running old daemon's legitimate servers.
func TestReapOrphanedServers_SkipsWhenDaemonAlive(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	other, otherPID := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(other) })
	startedAt, err := processStartTime(otherPID)
	if err != nil {
		t.Fatalf("processStartTime: %v", err)
	}

	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: otherPID, StartedAt: startedAt})

	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-999999.json", agent.ServerPIDInfo{
		PID:       999999,
		Agent:     "opencode",
		StartedAt: time.Now().UTC(),
	})

	reapOrphanedServers(p)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("pid file should be untouched when a daemon is alive, got err=%v", err)
	}
}

// TestReapOrphanedServers_SkipsKillWhenStartTimeMismatched simulates PID
// reuse: the PID file's StartedAt doesn't match the live process's actual
// start time, so the reaper must NOT send signals - it just drops the
// stale file.
func TestReapOrphanedServers_SkipsKillWhenStartTimeMismatched(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Bogus StartedAt far in the past makes this look like a reused PID.
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-reused.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: time.Now().UTC().Add(-24 * time.Hour),
	})

	reapOrphanedServers(p)

	// The process must still be alive - we didn't own it.
	alive, err := processRunning(pid)
	if err != nil {
		t.Fatalf("processRunning: %v", err)
	}
	if !alive {
		t.Error("reaper killed a process whose start time did not match the pid record")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale pid file should still be removed, got err=%v", err)
	}
}

func TestReapOrphanedServers_KeepsPIDFileWhenTerminateFails(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	started, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("read start time: %v", err)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-live.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: started,
	})

	old := terminateOrphanProcessGroupFunc
	terminateOrphanProcessGroupFunc = func(pid int, _ time.Time) error {
		return errors.New("boom")
	}
	t.Cleanup(func() { terminateOrphanProcessGroupFunc = old })

	reapOrphanedServers(p)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("pid file should be kept for retry after terminate failure, got err=%v", err)
	}
	if alive, err := processRunning(pid); err != nil {
		t.Fatalf("processRunning: %v", err)
	} else if !alive {
		t.Error("process should remain alive when terminate hook fails")
	}
}

func TestReapOrphanedServers_KeepsPIDFileWhenStartTimeCheckFails(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-live.json", agent.ServerPIDInfo{
		PID:       pid,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: time.Now().UTC(),
	})

	old := processStartTimeFunc
	processStartTimeFunc = func(gotPID int) (time.Time, error) {
		if gotPID != pid {
			t.Fatalf("unexpected pid %d", gotPID)
		}
		return time.Time{}, errors.New("boom")
	}
	t.Cleanup(func() { processStartTimeFunc = old })

	reapOrphanedServers(p)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("pid file should be kept for retry after start time check failure, got err=%v", err)
	}
	if alive, err := processRunning(pid); err != nil {
		t.Fatalf("processRunning: %v", err)
	} else if !alive {
		t.Error("process should remain alive when start time check fails")
	}
}

func TestReapOrphanedServers_ReapsWizardOwnedRecordWhenWizardGone(t *testing.T) {
	cmd, pid := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	started, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("read start time: %v", err)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	path := writePIDRecord(t, p.ServerPIDsDir(), "opencode-wizard-live.json", agent.ServerPIDInfo{
		PID:       pid,
		Owner:     agent.ServerPIDOwnerWizard,
		OwnerPID:  999999,
		Agent:     "opencode",
		Bin:       "/bin/sleep",
		StartedAt: started,
	})

	reapOrphanedServers(p)

	if !waitForPIDExit(pid, 5*time.Second) {
		t.Errorf("expected pid %d to be terminated", pid)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("pid file should be removed after reap, got err=%v", err)
	}
}

// spawnSigtermImmuneProcess launches a process group leader that ignores
// SIGTERM, so terminateOrphanProcessGroup is forced past its graceful-exit
// wait and into the SIGKILL path a test wants to observe. It returns only once
// the trap is installed: signalling before that races the shell's startup and
// kills it, which would make the test pass or fail for the wrong reason.
func spawnSigtermImmuneProcess(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "trapped")
	// A loop rather than a bare `sleep`: a shell may exec-replace itself with a
	// single trailing command, which would take the trap with it.
	script := `trap "" TERM; : > "$1"; while :; do sleep 1; done`
	cmd := exec.Command("/bin/sh", "-c", script, "sh", ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sigterm-immune process: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return cmd, cmd.Process.Pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	killAndWait(cmd)
	t.Fatal("sigterm-immune process never installed its trap")
	return nil, 0
}

// TestTerminateOrphanProcessGroup_RefusesNonGroupLeader pins the rule that the
// terminator never guesses a process group. Every leader recorded in a PID file
// is spawned with Setpgid and so is its own group leader; a pid that is not one
// has either been reused by an unrelated process or never belonged to us, and
// signalling -pgid there kills every member of a group we do not own. The
// original code fell back to pgid = pid whenever Getpgid failed, which made
// that outcome reachable rather than hypothetical.
//
// The non-leader here deliberately joins a throwaway group rather than
// inheriting the test runner's: if this guard ever regresses, the blast lands
// on that group instead of on the process running the suite.
func TestTerminateOrphanProcessGroup_RefusesNonGroupLeader(t *testing.T) {
	leader, leaderPID := spawnSleepProcess(t)
	t.Cleanup(func() { killAndWait(leader) })

	member := exec.Command("/bin/sh", "-c", "sleep 30")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leaderPID}
	if err := member.Start(); err != nil {
		t.Fatalf("start group member: %v", err)
	}
	pid := member.Process.Pid
	t.Cleanup(func() { killAndWait(member) })

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid != leaderPID {
		t.Fatalf("member pgid = %d, want the leader's group %d", pgid, leaderPID)
	}

	err = terminateOrphanProcessGroup(pid, time.Now().UTC())
	if err == nil {
		t.Fatal("expected a refusal for a pid that is not its own process-group leader")
	}
	if !strings.Contains(err.Error(), "not its own process-group leader") {
		t.Errorf("unexpected error: %v", err)
	}
	if alive, err := processRunning(pid); err != nil {
		t.Fatalf("processRunning: %v", err)
	} else if !alive {
		t.Error("terminator signalled a group it should have refused")
	}
}

// TestTerminateOrphanProcessGroup_SkipsSigkillWhenPIDReusedDuringWait covers
// the window the caller's pre-signal identity check cannot: the terminator
// waits up to three seconds between SIGTERM and SIGKILL, and a pid released in
// that window can be reused. Re-proving the recorded start time before SIGKILL
// is what stops the group kill from landing on the new owner.
func TestTerminateOrphanProcessGroup_SkipsSigkillWhenPIDReusedDuringWait(t *testing.T) {
	cmd, pid := spawnSigtermImmuneProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	// Stand in for "a different process now holds this pid": still alive, but
	// its start time no longer matches what the record was written with.
	oldStart := processStartTimeFunc
	processStartTimeFunc = func(gotPID int) (time.Time, error) {
		if gotPID != pid {
			t.Errorf("unexpected pid %d", gotPID)
		}
		return time.Now().UTC().Add(24 * time.Hour), nil
	}
	t.Cleanup(func() { processStartTimeFunc = oldStart })

	err := terminateOrphanProcessGroup(pid, time.Now().UTC().Add(-24*time.Hour))
	if err == nil {
		t.Fatal("expected a refusal once the pid no longer matches the record")
	}
	if !strings.Contains(err.Error(), "reused during termination") {
		t.Errorf("unexpected error: %v", err)
	}
	if alive, err := processRunning(pid); err != nil {
		t.Fatalf("processRunning: %v", err)
	} else if !alive {
		t.Error("sigkill was sent to a pid whose identity no longer matched the record")
	}
}

// TestTerminateOrphanProcessGroup_KillsSigtermImmuneGroup is the positive half:
// when identity still holds after the graceful-exit wait, SIGKILL goes to the
// whole group. Without it a leader that ignores SIGTERM survives the reap.
func TestTerminateOrphanProcessGroup_KillsSigtermImmuneGroup(t *testing.T) {
	cmd, pid := spawnSigtermImmuneProcess(t)
	t.Cleanup(func() { killAndWait(cmd) })

	started, err := processStartTime(pid)
	if err != nil {
		t.Fatalf("read start time: %v", err)
	}
	if err := terminateOrphanProcessGroup(pid, started); err != nil {
		t.Fatalf("terminateOrphanProcessGroup: %v", err)
	}
	if !waitForPIDExit(pid, 5*time.Second) {
		t.Errorf("expected pid %d to be killed after it ignored sigterm", pid)
	}
}
