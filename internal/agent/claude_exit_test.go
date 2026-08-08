package agent

import (
	"fmt"
	"strings"
	"testing"
)

// The claude harness writes its terminal refusals to STDOUT as ordinary
// assistant text, then exits nonzero with an empty stderr. These tests pin the
// two views claudeExitDetail must produce from that: everything the operator
// needs to read, and only the harness-controlled subset the retry loop may
// classify on.

func TestClaudeExitDetail_EmptyStderrStillCarriesTheAgentText(t *testing.T) {
	const stop = "You've hit your session limit - resets 3pm (America/Los_Angeles)"

	detail, harness := claudeExitDetail("", nil, "reviewing the diff...\n"+stop)

	if !strings.Contains(detail, stop) {
		t.Fatalf("detail must carry the stream diagnosis, got %q", detail)
	}
	if harness != "" {
		t.Errorf("agent text is not harness evidence, got harness %q", harness)
	}
}

func TestClaudeExitDetail_NoEvidenceAtAllSaysSoInsteadOfEmptyString(t *testing.T) {
	detail, harness := claudeExitDetail("", nil, "")

	if strings.TrimSpace(detail) == "" {
		t.Fatal("detail must never be empty: a bare trailing colon is undiagnosable")
	}
	if harness != "" {
		t.Errorf("expected empty harness evidence, got %q", harness)
	}
}

func TestClaudeExitDetail_StderrIsHarnessEvidence(t *testing.T) {
	const apiErr = `API Error: {"type":"error","error":{"type":"overloaded_error"}}`

	detail, harness := claudeExitDetail(apiErr+"\n", nil, "I could not reach the server")

	if !strings.Contains(detail, apiErr) {
		t.Errorf("detail must keep stderr, got %q", detail)
	}
	if !strings.Contains(harness, apiErr) {
		t.Errorf("stderr must stay classifiable, got harness %q", harness)
	}
	if strings.Contains(harness, "could not reach") {
		t.Errorf("agent text must not leak into harness evidence, got %q", harness)
	}
}

func TestClaudeExitDetail_ResultEventFieldsAreHarnessEvidence(t *testing.T) {
	result := &claudeResult{Subtype: "error_during_execution", IsError: true, finalText: "tool loop aborted"}

	detail, harness := claudeExitDetail("", result, "")

	for _, want := range []string{"error_during_execution", "is_error=true", "tool loop aborted"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q, got %q", want, detail)
		}
	}
	if !strings.Contains(harness, "error_during_execution") || !strings.Contains(harness, "is_error=true") {
		t.Errorf("subtype/is_error must stay classifiable, got harness %q", harness)
	}
	if strings.Contains(harness, "tool loop aborted") {
		t.Errorf("the result event's own text is agent output, got harness %q", harness)
	}
}

func TestClaudeExitDetail_DoesNotRepeatTheFinalMessageTwice(t *testing.T) {
	const msg = "the only thing it said"
	result := &claudeResult{Subtype: "success", finalText: msg}

	detail, _ := claudeExitDetail("", result, "warming up. "+msg)

	if n := strings.Count(detail, msg); n != 1 {
		t.Errorf("expected the final message once, got %d in %q", n, detail)
	}
}

func TestClaudeExitDetail_BoundsTheAgentTextAndKeepsTheTail(t *testing.T) {
	const stop = "STOPPED: you hit your limit"
	text := strings.Repeat("chatter ", claudeExitDetailMaxBytes) + stop

	detail, _ := claudeExitDetail("", nil, text)

	if !strings.Contains(detail, stop) {
		t.Error("the diagnosis lands at the END of the stream, so the tail must be kept")
	}
	if len(detail) > 4*claudeExitDetailMaxBytes {
		t.Errorf("detail must stay bounded, got %d bytes", len(detail))
	}
}

func TestClaudeExitDetail_TruncationKeepsValidUTF8(t *testing.T) {
	// A THREE-byte rune, deliberately: claudeExitDetailMaxBytes is even, so a
	// two-byte rune leaves the cut on a boundary and the test would pass with
	// the rune-boundary loop deleted. 2000 is not divisible by 3, so this one
	// forces the tail to start mid-rune.
	text := strings.Repeat("€", claudeExitDetailMaxBytes)

	detail, _ := claudeExitDetail("", nil, text)

	if strings.ContainsRune(detail, '�') {
		t.Errorf("truncation split a rune: %q", detail)
	}
}

func TestClassifyTransient_ScopedErrorReadsOnlyHarnessEvidence(t *testing.T) {
	// The model typing a transient-sounding phrase must not buy another attempt.
	prose := transientScoped(
		fmt.Errorf("claude exited: exit status 1: agent output (tail): I got connection refused from the sandbox"),
		"exit status 1",
	)
	if label, retry := classifyTransient(prose); retry {
		t.Errorf("agent prose must not be classified transient, got %q", label)
	}

	// Harness evidence still classifies exactly as before.
	real := transientScoped(
		fmt.Errorf("claude exited: exit status 1: overloaded_error; agent output (tail): all fine here"),
		`exit status 1; API Error: {"type":"overloaded_error"}`,
	)
	label, retry := classifyTransient(real)
	if !retry || label != "overloaded_error" {
		t.Errorf("harness evidence must stay classifiable, got (%q, %v)", label, retry)
	}
}

func TestClaudeRetryClassifier_SessionLimitStopIsNotRetried(t *testing.T) {
	// The robots-618i incident: empty stderr, no result event, and a stop
	// message that no amount of immediate retrying can clear.
	detail, harness := claudeExitDetail("", nil, "You've hit your session limit - resets 3pm")
	err := transientScoped(fmt.Errorf("claude exited: exit status 1: %s", detail), "exit status 1; "+harness)

	if label, retry := claudeRetryClassifier(err); retry {
		t.Errorf("a session limit is not a transient blip, got %q", label)
	}
}
