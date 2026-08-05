package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// terminateShellCommandGroupFunc is a seam: the group kill has no in-process
// failure mode a test can provoke, but the record-retention branch it guards is
// exactly the behaviour worth pinning.
var terminateShellCommandGroupFunc = shellenv.TerminateShellCommandGroup

type nativeAgentCommand struct {
	cmd            *exec.Cmd
	pidFile        string
	stdout         *nativeAgentPipe
	stderr         *nativeAgentPipe
	waitCh         chan error
	terminateOnce  sync.Once
	closePipesOnce sync.Once
	pipeMu         sync.Mutex
	remainingPipes int
	pipesDone      chan struct{}
}

type nativeAgentPipe struct {
	file     *os.File
	done     func()
	doneOnce sync.Once
}

func (p *nativeAgentPipe) Read(b []byte) (int, error) {
	n, err := p.file.Read(b)
	if err != nil {
		p.markDone()
	}
	return n, err
}

func (p *nativeAgentPipe) Close() error {
	err := p.file.Close()
	p.markDone()
	return err
}

func (p *nativeAgentPipe) markDone() {
	p.doneOnce.Do(p.done)
}

// startNativeAgentCommand starts a native agent leader that was already
// prepared with shellenv.ConfigureShellCommand, and records it for crash
// recovery.
//
// In-process reaping (cmd.Cancel on cancellation, terminate on every exit
// path) covers only the case where our own Go code still gets to run. When the
// daemon itself dies uncatchably - the OOM kill described on
// TerminateShellCommandGroup, a `kill -9`, or a session teardown that signals
// the daemon but not its descendants - nothing signals the agent's process
// group, so the leader and everything it spawned reparent to init and run
// forever. Managed servers already survived that through an on-disk PID record
// reaped by the next daemon start; native agents did not, and their trees are
// the ones that spawn test runners. agentName tags the record so that reap can
// name what it killed.
func startNativeAgentCommand(agentName string, cmd *exec.Cmd) (*nativeAgentCommand, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := shellenv.StartShellCommand(cmd); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	started := &nativeAgentCommand{
		cmd: cmd,
		// Written immediately after Start so the window in which a daemon
		// death leaves an untracked tree is as small as the managed-server
		// path's. Empty when PID tracking is disabled (no daemon identity),
		// which makes every use below a no-op.
		pidFile: writeServerPIDFile(currentServerPIDsDir(), ServerPIDInfo{
			PID:            cmd.Process.Pid,
			Owner:          currentServerPIDOwner(),
			OwnerPID:       os.Getpid(),
			OwnerStartedAt: CurrentProcessStartedAt(),
			Agent:          agentName,
			Bin:            cmd.Path,
			StartedAt:      time.Now().UTC(),
		}),
		waitCh:         make(chan error, 1),
		remainingPipes: 2,
		pipesDone:      make(chan struct{}),
	}
	started.stdout = &nativeAgentPipe{file: stdoutR, done: started.markPipeDone}
	started.stderr = &nativeAgentPipe{file: stderrR, done: started.markPipeDone}
	go func() {
		err := cmd.Wait()
		started.terminate()
		started.waitCh <- started.waitForPipes(err)
	}()
	return started, nil
}

func (c *nativeAgentCommand) markPipeDone() {
	c.pipeMu.Lock()
	defer c.pipeMu.Unlock()
	c.remainingPipes--
	if c.remainingPipes == 0 {
		close(c.pipesDone)
	}
}

func (c *nativeAgentCommand) pid() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *nativeAgentCommand) waitForPipes(waitErr error) error {
	if c.cmd.WaitDelay <= 0 {
		<-c.pipesDone
		return waitErr
	}
	timer := time.NewTimer(c.cmd.WaitDelay)
	defer timer.Stop()
	select {
	case <-c.pipesDone:
		return waitErr
	case <-timer.C:
		c.closePipes()
		if waitErr == nil {
			return exec.ErrWaitDelay
		}
		return waitErr
	}
}

// terminate SIGKILLs the leader's whole process group and drops the
// crash-recovery record only once that kill reports success. The order and the
// condition both matter: the record must outlive every path on which the group
// might still have survivors. A successful group kill is proof there are none -
// SIGKILL is uncatchable - so the record has nothing left to describe. A failed
// one is the opposite: descendants may still be running and this record is the
// only handle a future daemon has on them, so it stays. Keeping it costs
// nothing when the leader did die anyway, because reapOrphanedServers discards
// records whose PID is dead or has been reused without signalling anything.
func (c *nativeAgentCommand) terminate() {
	c.terminateOnce.Do(func() {
		if err := terminateShellCommandGroupFunc(c.cmd); err != nil {
			slog.Warn("terminate native agent process group; keeping pid record for crash recovery",
				"pid", c.pid(), "pid_file", c.pidFile, "error", err)
			return
		}
		removeServerPIDFile(c.pidFile)
	})
}

func (c *nativeAgentCommand) waitAfterParseError(parseErr error) error {
	c.terminate()
	c.closePipes()
	waitErr := c.wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return waitErr
	}
	return parseErr
}

func (c *nativeAgentCommand) wait() error {
	return <-c.waitCh
}

func (c *nativeAgentCommand) closePipes() {
	c.closePipesOnce.Do(func() {
		_ = c.stdout.Close()
		_ = c.stderr.Close()
	})
}
