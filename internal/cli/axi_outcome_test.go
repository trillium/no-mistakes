package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// robots-618i: a step agent that died AFTER committing its fix reported the run
// as a flat `failed`. An agent reading that concludes its fix never applied and
// re-does work the pipeline already preserved - exactly what
// preserveGateFixCommitsGuidance warns against. The custody facts that say
// otherwise already exist in branch_sync; the outcome word must agree with them.

func recoverableState() *branchsync.State {
	state := &branchsync.State{State: branchsync.StatePipelineOwned}
	state.Safety = "blocked_pipeline_owned_recoverable"
	return state
}

func TestOutcomeForRun_FailedWithPreservedWorkIsNotFlatFailed(t *testing.T) {
	got := outcomeForRun(string(types.RunFailed), recoverableState())

	if got != outcomeFailedWorkPreserved {
		t.Fatalf("outcomeForRun = %q, want %q", got, outcomeFailedWorkPreserved)
	}
}

func TestOutcomeForRun_FailedWithNothingCommittedStaysFailed(t *testing.T) {
	// The run ended before it moved the head: nothing was preserved, so the
	// plain word is the honest one.
	released := &branchsync.State{State: branchsync.StateUserOwned}
	released.Safety = "user_owned"

	for name, state := range map[string]*branchsync.State{
		"no custody facts": nil,
		"branch released":  released,
	} {
		if got := outcomeForRun(string(types.RunFailed), state); got != "failed" {
			t.Errorf("%s: outcomeForRun = %q, want %q", name, got, "failed")
		}
	}
}

func TestOutcomeForRun_ActiveRunCustodyDoesNotRenameTheOutcome(t *testing.T) {
	// blocked_pipeline_owned (run still running) is a different state: the head
	// may still move, and nothing terminal has been preserved.
	active := &branchsync.State{State: branchsync.StatePipelineOwned}
	active.Safety = "blocked_pipeline_owned"

	if got := outcomeForRun(string(types.RunFailed), active); got != "failed" {
		t.Errorf("outcomeForRun = %q, want %q", got, "failed")
	}
}

func TestOutcomeForRun_NonFailedStatusesAreUnchangedByCustody(t *testing.T) {
	// Preserved custody explains a FAILURE. It must not relabel a run that
	// passed, and cancellation already has its own word.
	for status, want := range map[string]string{
		string(types.RunCompleted): "passed",
		string(types.RunCancelled): "cancelled",
	} {
		if got := outcomeForRun(status, recoverableState()); got != want {
			t.Errorf("outcomeForRun(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestFailedWorkPreservedHelpNamesTheRecoveryAndForbidsRedoingTheWork(t *testing.T) {
	joined := strings.Join(failedWorkPreservedHelp(), "\n")

	for _, want := range []string{"recover_custody", "already", "committed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("help must mention %q, got %q", want, joined)
		}
	}
}
