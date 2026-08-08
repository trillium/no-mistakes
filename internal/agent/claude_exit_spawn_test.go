//go:build unix

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeClaudeScript writes a stub `claude` whose body is the given POSIX
// shell script, so a test can reproduce an exact stdout/stderr/exit-code shape.
func writeFakeClaudeScript(t *testing.T, script string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return bin
}

// TestClaudeAgent_RunTurn_SessionLimitStopIsDiagnosable reproduces robots-618i:
// claude announces a session limit as ordinary assistant text on STDOUT, emits
// no result event, and exits 1 with an EMPTY stderr. The pipeline used to report
// `claude exited: exit status 1: ` - a trailing colon with nothing after it, and
// no way to tell an OOM from a rate limit from a clean-but-nonzero exit.
func TestClaudeAgent_RunTurn_SessionLimitStopIsDiagnosable(t *testing.T) {
	bin := writeFakeClaudeScript(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"assistant","session_id":"s1","message":{"model":"claude-opus-5","usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"You have hit your session limit - resets 3pm (America/Los_Angeles)"}]}}'
exit 1
`)
	a := &claudeAgent{bin: bin}

	_, _, _, err := a.runTurn(context.Background(), "review the diff", RunOpts{CWD: t.TempDir()}, "")
	if err == nil {
		t.Fatal("expected a nonzero-exit error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "session limit") {
		t.Errorf("error must name what claude actually said, got %q", msg)
	}
	if strings.HasSuffix(strings.TrimSpace(msg), ":") {
		t.Errorf("error must not end in a bare colon, got %q", msg)
	}
	if label, retry := claudeRetryClassifier(err); retry {
		t.Errorf("a session limit must not be retried, got %q", label)
	}
}

// A transient API error still arrives on stderr, still reaches the operator, and
// still earns the retry it earned before.
func TestClaudeAgent_RunTurn_TransientStderrStaysRetriable(t *testing.T) {
	bin := writeFakeClaudeScript(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' 'API Error: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}' >&2
exit 1
`)
	a := &claudeAgent{bin: bin}

	_, _, _, err := a.runTurn(context.Background(), "review the diff", RunOpts{CWD: t.TempDir()}, "")
	if err == nil {
		t.Fatal("expected a nonzero-exit error")
	}
	if !strings.Contains(err.Error(), "overloaded_error") {
		t.Errorf("stderr must still reach the operator, got %q", err.Error())
	}
	label, retry := claudeRetryClassifier(err)
	if !retry || label != "overloaded_error" {
		t.Errorf("expected an overloaded_error retry, got (%q, %v)", label, retry)
	}
}

// A clean exit that produced no result event is the same blind spot one step
// earlier: say what the process did emit instead of only that it emitted nothing.
func TestClaudeAgent_RunTurn_MissingResultEventCarriesWhatWasSaid(t *testing.T) {
	bin := writeFakeClaudeScript(t, `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"assistant","session_id":"s1","message":{"usage":{"input_tokens":1,"output_tokens":1},"content":[{"type":"text","text":"I stopped early because the sandbox is read-only"}]}}'
exit 0
`)
	a := &claudeAgent{bin: bin}

	_, _, _, err := a.runTurn(context.Background(), "review the diff", RunOpts{CWD: t.TempDir()}, "")
	if err == nil {
		t.Fatal("expected a no-result-event error")
	}
	if !strings.Contains(err.Error(), "read-only") {
		t.Errorf("error must carry the agent's own account, got %q", err.Error())
	}
}
