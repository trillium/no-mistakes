package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// startNotifyPushDaemon serves push_received with the supplied handler on a real
// socket under a fresh NM_HOME, so `daemon notify-push` runs end to end.
func startNotifyPushDaemon(t *testing.T, handler ipc.HandlerFunc) {
	t.Helper()
	// HOME is isolated alongside NM_HOME: anything that falls back to the real
	// home would read the developer's own daemon state and config.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("NM_HOME", makeSocketSafeTempDir(t))

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodPushReceived, handler)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(p.Socket()) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("fake daemon did not stop")
		}
	})
	// A socket that never accepts would otherwise leave every test below
	// asserting against a connect failure instead of the behaviour it names.
	ready := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client, dialErr := ipc.Dial(p.Socket()); dialErr == nil {
			client.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("fake daemon socket %s never accepted a connection", p.Socket())
	}
}

func runNotifyPush(t *testing.T, gate string) (string, error) {
	t.Helper()
	cmd := newDaemonNotifyPushCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{
		"--gate", gate,
		"--ref", "refs/heads/feature/x",
		"--old", "0000000000000000000000000000000000000000",
		"--new", "5cd6827ceff24ee31b2ff45875a902e4840f69ad",
	})
	err := cmd.Execute()
	return out.String(), err
}

// The ref is already stored by the time the daemon stops answering, so a
// missing confirmation must not present as a failed push. It exits 0, says the
// run state is unconfirmed, and names the check and recovery commands instead
// of the old bare exit-1 socket error (robots-8bao).
func TestNotifyPushUnconfirmedResponseIsNotAPushFailure(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	startNotifyPushDaemon(t, func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return &ipc.PushReceivedResult{RunID: "run-late"}, nil
	})

	previous := notifyPushCallTimeout
	notifyPushCallTimeout = 100 * time.Millisecond
	t.Cleanup(func() { notifyPushCallTimeout = previous })

	out, err := runNotifyPush(t, t.TempDir())
	if err != nil {
		t.Fatalf("notify-push returned an error for a delivered push: %v\n%s", err, out)
	}
	for _, want := range []string{
		"did not confirm",
		"The push itself succeeded",
		"unconfirmed",
		"no-mistakes axi status",
		"no-mistakes axi run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("advisory missing %q:\n%s", want, out)
		}
	}
}

// A confirmed notification stays silent: the hook prints its own banner and any
// output at all is what tells it the run is unconfirmed.
func TestNotifyPushConfirmedResponseIsSilent(t *testing.T) {
	startNotifyPushDaemon(t, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.PushReceivedResult{RunID: "run-ok"}, nil
	})

	out, err := runNotifyPush(t, t.TempDir())
	if err != nil {
		t.Fatalf("notify-push: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("confirmed notify-push wrote output:\n%s", out)
	}
}

// Only an unanswered request is degraded. A daemon that answers with an error
// has told us the run does NOT exist, so that must still fail the notification.
func TestNotifyPushDaemonErrorStillFails(t *testing.T) {
	startNotifyPushDaemon(t, func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, context.DeadlineExceeded
	})

	out, err := runNotifyPush(t, t.TempDir())
	if err == nil {
		t.Fatalf("notify-push succeeded despite a daemon-reported failure:\n%s", out)
	}
	if strings.Contains(out, "no-mistakes axi run") {
		t.Errorf("daemon-reported failure rendered the unconfirmed advisory:\n%s", out)
	}
}
