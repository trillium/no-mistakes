package ipc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// A request that was written but never answered is a fundamentally different
// outcome from one that never reached the daemon: the daemon owns the work, only
// the confirmation is missing. Callers that must not misreport delivered work as
// failed work (the post-receive hook, robots-8bao) need to tell them apart.
func TestCallResponseTimeoutIsDistinguishableFromOtherFailures(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	srv.Handle("slow", func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		select {
		case <-released:
		case <-ctx.Done():
		}
		return map[string]string{"ok": "yes"}, nil
	})
	srv.Handle("boom", func(context.Context, json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("handler said no")
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result json.RawMessage
	err = c.CallWithTimeout("slow", nil, &result, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the daemon never answers")
	}
	if !ipc.IsResponseTimeout(err) {
		t.Fatalf("error %T %v, want a response timeout", err, err)
	}
	var timeoutErr *ipc.ResponseTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error %v does not unwrap to *ResponseTimeoutError", err)
	}
	if timeoutErr.Method != "slow" {
		t.Errorf("Method = %q, want %q", timeoutErr.Method, "slow")
	}
	if timeoutErr.TimeoutDuration != 50*time.Millisecond {
		t.Errorf("TimeoutDuration = %v, want 50ms", timeoutErr.TimeoutDuration)
	}
	if !timeoutErr.Timeout() {
		t.Error("ResponseTimeoutError.Timeout() = false, want true")
	}

	// A daemon that answers with an error answered: not a response timeout.
	c2, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c2.Close()
	err = c2.Call("boom", nil, &result)
	if err == nil {
		t.Fatal("expected an RPC error")
	}
	if ipc.IsResponseTimeout(err) {
		t.Fatalf("RPC error %v misclassified as a response timeout", err)
	}
}

// A cancelled context must keep reporting cancellation. The cancel path works by
// forcing the read deadline to now, which produces the same underlying i/o
// timeout as a real deadline expiry - so the two must not be conflated.
func TestCancelledCallIsNotAResponseTimeout(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	srv.Handle("slow", func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
		select {
		case <-released:
		case <-ctx.Done():
		}
		return map[string]string{"ok": "yes"}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	var result json.RawMessage
	err = c.CallWithContext(ctx, "slow", nil, &result, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %T %v, want context.Canceled", err, err)
	}
	if ipc.IsResponseTimeout(err) {
		t.Fatalf("cancellation %v misclassified as a response timeout", err)
	}
}
