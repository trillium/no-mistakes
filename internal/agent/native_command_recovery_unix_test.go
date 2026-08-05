//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const nativeAgentRecoveryHelperEnv = "NM_AGENT_NATIVE_RECOVERY_HELPER"

// TestStartNativeAgentCommand_RecordsLeaderUntilItsGroupIsReaped is the
// regression test for the day-long orphan trees observed on a developer
// machine: two `branchsync.test` and two `steps.test` binaries with ppid=1,
// ~205% CPU between them and roughly 24 CPU-hours burned, each one having
// blown through its own -test.timeout=10m by up to 120x.
//
// Those were grandchildren of a Test-step agent. Every in-process reaping path
// the repo already had - cmd.Cancel on cancellation, terminate on clean exit,
// error, and parse failure - requires our own Go code to run. None of them fire
// when the daemon dies uncatchably, and then the agent leader plus every test
// runner under it reparents to init with nothing left that knows their pgid.
// Managed servers already survived a daemon crash through an on-disk record
// reaped at the next daemon start; native agents did not.
//
// The invariant this pins: from the moment a native agent leader starts until
// the moment its process group has actually been killed, a crash-recovery
// record naming its PID exists on disk. A daemon that dies anywhere inside that
// window leaves a successor the one thing it needs to reap the tree.
func TestStartNativeAgentCommand_RecordsLeaderUntilItsGroupIsReaped(t *testing.T) {
	pidsDir := t.TempDir()
	SetServerPIDsDirForOwner(pidsDir, ServerPIDOwnerDaemon)
	t.Cleanup(func() { SetServerPIDsDirForOwner("", "") })

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentRecoveryHelper$")
	cmd.Env = append(os.Environ(), nativeAgentRecoveryHelperEnv+"=sleep")
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand("claude", cmd)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	leader := started.pid()
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		started.closePipes()
	})

	info := readSoleServerPIDRecord(t, pidsDir)
	if info.PID != leader {
		t.Fatalf("recorded pid = %d, want the live agent leader %d", info.PID, leader)
	}
	if info.Agent != "claude" {
		t.Fatalf("recorded agent = %q, want claude", info.Agent)
	}
	if info.Owner != ServerPIDOwnerDaemon {
		t.Fatalf("recorded owner = %q, want %q", info.Owner, ServerPIDOwnerDaemon)
	}
	if info.OwnerPID != os.Getpid() {
		t.Fatalf("recorded owner pid = %d, want %d", info.OwnerPID, os.Getpid())
	}
	if info.Bin != cmd.Path {
		t.Fatalf("recorded bin = %q, want %q", info.Bin, cmd.Path)
	}
	// The reaper refuses to signal a PID whose kernel start time disagrees with
	// the record, so an unset or wildly wrong StartedAt would make recovery a
	// silent no-op rather than a kill.
	if drift := time.Since(info.StartedAt); drift < 0 || drift > time.Minute {
		t.Fatalf("recorded start time %v is not close to now (drift %v)", info.StartedAt, drift)
	}

	started.terminate()

	entries, err := os.ReadDir(pidsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("pid records remain after the group was reaped: %d", len(entries))
	}
}

// TestStartNativeAgentCommand_NoRecordWithoutDaemonIdentity keeps the blast
// radius where it belongs: a CLI or test process that runs an agent without a
// daemon identity has no successor that could safely reap on its behalf, so it
// must not leave records a real daemon would later act on.
func TestStartNativeAgentCommand_NoRecordWithoutDaemonIdentity(t *testing.T) {
	SetServerPIDsDirForOwner("", "")

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentRecoveryHelper$")
	cmd.Env = append(os.Environ(), nativeAgentRecoveryHelperEnv+"=exit")
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand("codex", cmd)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	if started.pidFile != "" {
		t.Fatalf("pid file = %q, want none when tracking is disabled", started.pidFile)
	}
	started.terminate()
	started.closePipes()
	_ = started.wait()
}

// TestStartNativeAgentCommand_KeepsRecordWhenGroupKillFails covers the other
// half of the invariant above. Dropping the record is only safe because a
// successful group kill is proof there is nothing left to reap - SIGKILL cannot
// be caught. When the kill reports failure that proof is gone: descendants may
// still be running, and this record is the only handle a future daemon has on
// their process group. Deleting it there would reintroduce exactly the untracked
// tree the fix exists to prevent.
func TestStartNativeAgentCommand_KeepsRecordWhenGroupKillFails(t *testing.T) {
	pidsDir := t.TempDir()
	SetServerPIDsDirForOwner(pidsDir, ServerPIDOwnerDaemon)
	t.Cleanup(func() { SetServerPIDsDirForOwner("", "") })

	oldTerminate := terminateShellCommandGroupFunc
	terminateShellCommandGroupFunc = func(*exec.Cmd) error {
		return errors.New("group kill failed")
	}
	t.Cleanup(func() { terminateShellCommandGroupFunc = oldTerminate })

	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestNativeAgentRecoveryHelper$")
	cmd.Env = append(os.Environ(), nativeAgentRecoveryHelperEnv+"=sleep")
	shellenv.ConfigureShellCommand(cmd)

	started, err := startNativeAgentCommand("claude", cmd)
	if err != nil {
		t.Fatalf("startNativeAgentCommand: %v", err)
	}
	leader := started.pid()
	t.Cleanup(func() {
		_ = syscall.Kill(-leader, syscall.SIGKILL)
		started.closePipes()
	})

	started.terminate()

	info := readSoleServerPIDRecord(t, pidsDir)
	if info.PID != leader {
		t.Fatalf("recorded pid = %d, want the still-unreaped leader %d", info.PID, leader)
	}
}

func readSoleServerPIDRecord(t *testing.T, dir string) ServerPIDInfo {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("pid records in %s = %d, want exactly 1", dir, len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var info ServerPIDInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("unmarshal %s: %v", entries[0].Name(), err)
	}
	return info
}

func TestNativeAgentRecoveryHelper(t *testing.T) {
	switch os.Getenv(nativeAgentRecoveryHelperEnv) {
	case "sleep":
		// Long enough that the parent test always observes a live leader, and
		// bounded so a failed run cannot leak this process indefinitely.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "exit":
		_, _ = os.Stdout.WriteString("pid " + strconv.Itoa(os.Getpid()) + "\n")
		os.Exit(0)
	}
}
