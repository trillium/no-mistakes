package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// gitRunner runs a git subcommand and returns trimmed stdout. Callers bind it
// to the right working directory and environment (git.Run for the push step,
// stepGitRun for the CI step so test env overrides apply).
type gitRunner func(args ...string) (string, error)

// forcePushDecision describes how to push a head to a remote branch safely.
// Exactly one of newBranch / upToDate is true, or neither (a guarded
// force-push anchored to remoteSHA is required).
type forcePushDecision struct {
	remoteSHA string // current remote head; the lease anchor for a force-push
	newBranch bool   // the branch does not exist on the remote -> plain push
	upToDate  bool   // the remote already points at the head -> no push needed
}

// forcePushWouldDiscardError reports that a force-push would discard commits
// present on the remote branch that the pipeline never incorporated. Refusing
// is the whole point: it is what keeps a stale-base rebase or an out-of-band
// push from silently dropping work that already landed upstream.
type forcePushWouldDiscardError struct {
	ref       string
	remoteSHA string
	dropped   []string
}

func (e *forcePushWouldDiscardError) Error() string {
	sample := e.dropped
	if len(sample) > 5 {
		sample = sample[:5]
	}
	return fmt.Sprintf(
		"refusing to force-push %s: remote head %s carries %d commit(s) the pipeline never incorporated (e.g. %s); pushing would discard upstream work. Re-fetch and rebase onto the current remote, or push manually if this overwrite is intended.",
		e.ref, shortSHA(e.remoteSHA), len(e.dropped), strings.Join(shortSHAs(sample), ", "),
	)
}

// resolveForcePushDecision re-reads the current state of ref on the push remote
// and decides whether force-pushing newHeadSHA would discard commits the
// pipeline never saw. It returns a decision the caller acts on, or a non-nil
// error when the push must NOT proceed (either git failed, in which case we
// fail safe rather than degrade to a blind --force, or the push would discard
// unseen upstream commits).
//
// lastSeenSHA is the remote head the pipeline last observed for this branch:
//   - push step: the remote-tracking ref synced by the rebase step. On a normal
//     push that is the exact commit the branch was rebased against; on a force
//     push the rebase step deliberately leaves it stale, so it stays the head we
//     last observed rather than the live tip - which is what keeps the fast path
//     below honest (see the rebase step comment) and forces the content check.
//   - CI step: the run's last-recorded head, i.e. what the pipeline last pushed.
//
// When the remote still points there the push is safe (it only fast-forwards or
// replays our own prior state). Otherwise the remote moved out of band and the
// push is allowed only when every commit now on the remote is already
// incorporated, by content (patch-id), into newHeadSHA, or is part of the
// history the run already knew (reachable from baseSHA) and is thus a deliberate
// rewrite rather than a clobber. Anything else is refused rather than discarded.
func resolveForcePushDecision(gitRun gitRunner, pushURL, ref, newHeadSHA, lastSeenSHA, baseSHA string) (forcePushDecision, error) {
	current, err := lsRemoteSHA(gitRun, pushURL, ref)
	if err != nil {
		return forcePushDecision{}, fmt.Errorf("resolve remote head for %s: %w", ref, err)
	}
	if current == "" {
		return forcePushDecision{newBranch: true}, nil
	}
	if current == newHeadSHA {
		return forcePushDecision{remoteSHA: current, upToDate: true}, nil
	}
	if lastSeenSHA != "" && current == lastSeenSHA {
		// Remote unchanged since the pipeline last observed it: the force-push
		// only rewrites history we built on or last produced ourselves.
		return forcePushDecision{remoteSHA: current}, nil
	}
	// The remote moved to a commit we did not produce. Allow the push only if
	// everything now on the remote is already represented in what we are about
	// to push (or in the history the run is knowingly rewriting); otherwise
	// refuse rather than discard it.
	dropped, err := remoteCommitsNotIncorporated(gitRun, pushURL, ref, newHeadSHA, current, baseSHA)
	if err != nil {
		return forcePushDecision{}, fmt.Errorf("verify force-push safety for %s: %w", ref, err)
	}
	if len(dropped) == 0 {
		return forcePushDecision{remoteSHA: current}, nil
	}
	return forcePushDecision{}, &forcePushWouldDiscardError{ref: ref, remoteSHA: current, dropped: dropped}
}

// submittedWorkDroppedError reports that the head about to be pushed does not
// carry the changes the run was submitted with.
type submittedWorkDroppedError struct {
	newHeadSHA    string
	submittedHead string
	dropped       []string
}

func (e *submittedWorkDroppedError) Error() string {
	sample := e.dropped
	if len(sample) > 5 {
		sample = sample[:5]
	}
	return fmt.Sprintf(
		"refusing to push %s: it drops %d commit(s) from the submitted head %s (e.g. %s) whose changes are not present in what would be pushed; the branch would be replaced rather than advanced. Commit fixes ON TOP of the current branch head - rebasing is fine, replacing the branch is not. If the submitted work is itself wrong, report it as a finding instead of rewriting it away.",
		shortSHA(e.newHeadSHA), len(e.dropped), shortSHA(e.submittedHead), strings.Join(shortSHAs(sample), ", "),
	)
}

// assertSubmittedWorkPreserved refuses a push whose head no longer carries the
// changes the run was submitted with.
//
// resolveForcePushDecision protects the branch only against commits that reached
// the remote OUT OF BAND: when the remote still points at the head the pipeline
// itself last pushed (`current == lastSeenSHA`) it takes the fast path and never
// looks at content at all. That is correct for its own question and useless for
// this one, because the pipeline is then free to push anything - including a
// head built straight on the base branch, with every author commit gone. That is
// robots-1o2m: a CI fix agent reset the worktree to the base branch, committed an
// 8-line rewrite, and the pipeline force-replaced the branch, dropping the
// author's guard, their regression test, and the pipeline's own document commit
// while leaving the PR looking green.
//
// The comparison is by patch-id (`--cherry-pick`), never literal ancestry: the
// merge-conflict CI fix prompt explicitly asks the agent to REBASE onto the base
// branch, which rewrites every SHA on the branch, so requiring submittedHead to
// be an ancestor would refuse the exact repair the pipeline requested. A replayed
// commit keeps its patch-id and is recognized as preserved; a commit that only
// ever existed on the submitted head is reported as about to be dropped. A
// deliberate reversal of submitted work stays allowed, because a revert leaves
// the reverted commit in history rather than removing it.
//
// Fails closed: an unresolvable submitted head or a failing rev-list refuses the
// push rather than degrading to no check, because this guard is the last thing
// standing between an agent's history rewrite and the author's work.
func assertSubmittedWorkPreserved(gitRun gitRunner, newHeadSHA, submittedHead string) error {
	submittedHead = strings.TrimSpace(submittedHead)
	if submittedHead == "" || git.IsZeroSHA(submittedHead) || submittedHead == newHeadSHA {
		return nil
	}
	if _, err := gitRun("rev-parse", "--verify", "--quiet", submittedHead+"^{commit}"); err != nil {
		return fmt.Errorf("refusing to push %s: cannot resolve the submitted head %s to verify the run's own work is preserved: %w",
			shortSHA(newHeadSHA), shortSHA(submittedHead), err)
	}
	out, err := gitRun("rev-list", "--cherry-pick", "--right-only", newHeadSHA+"..."+submittedHead)
	if err != nil {
		return fmt.Errorf("verify submitted work is preserved in %s: %w", shortSHA(newHeadSHA), err)
	}
	var dropped []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			dropped = append(dropped, line)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	return &submittedWorkDroppedError{newHeadSHA: newHeadSHA, submittedHead: submittedHead, dropped: dropped}
}

// lsRemoteSHA returns the SHA a ref resolves to on a remote, or "" when the ref
// does not exist there.
func lsRemoteSHA(gitRun gitRunner, remote, ref string) (string, error) {
	out, err := gitRun("ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// remoteCommitsNotIncorporated returns the commits reachable from remoteSHA
// whose changes are not already present (by patch-id) in newHeadSHA and are not
// part of the history the run already knew (reachable from baseSHA). It first
// fetches the remote branch tip into FETCH_HEAD so the commits are available
// locally for the comparison, without disturbing remote-tracking refs.
//
// Using --cherry-pick (patch-id equivalence) rather than a plain ancestry check
// is what makes this correct across rebases: a clean rebase rewrites commit
// SHAs but preserves patch-ids, so commits the pipeline genuinely replayed are
// recognized as incorporated, while a commit that only ever existed on the
// remote is reported as about-to-be-discarded.
//
// Excluding baseSHA's ancestors (^baseSHA) is what lets a force push legitimately
// rewrite history the gate already knew - the common amend, or reverting the
// pipeline's own prior autofix commit - without flagging it as data loss, while
// still catching a commit that reached the branch out of band (which, having
// arrived after the run's base, is never an ancestor of baseSHA). baseSHA is
// omitted when empty/zero or not resolvable locally, which only makes the check
// stricter (more likely to refuse), never laxer.
func remoteCommitsNotIncorporated(gitRun gitRunner, pushURL, ref, newHeadSHA, remoteSHA, baseSHA string) ([]string, error) {
	branch := strings.TrimPrefix(ref, "refs/heads/")
	if _, err := gitRun("fetch", "--no-tags", pushURL, "refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("fetch remote branch: %w", err)
	}
	args := []string{"rev-list", "--cherry-pick", "--right-only", newHeadSHA + "..." + remoteSHA}
	if baseSHA != "" && !git.IsZeroSHA(baseSHA) {
		if _, err := gitRun("rev-parse", "--verify", "--quiet", baseSHA+"^{commit}"); err == nil {
			args = append(args, "^"+baseSHA)
		}
	}
	out, err := gitRun(args...)
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

func shortSHAs(shas []string) []string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = shortSHA(s)
	}
	return out
}
