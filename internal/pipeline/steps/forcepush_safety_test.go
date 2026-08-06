package steps

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// fileAtRef reports whether path exists in the tree at ref in the given repo.
func fileAtRef(t *testing.T, dir, ref, path string) bool {
	t.Helper()
	cmd := exec.Command("git", "cat-file", "-e", ref+":"+path)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// Issue #281: after no-mistakes opens a PR, a reviewed commit is pushed to
// origin only (not through the gate). When main moves and the CI monitor
// auto-fixes the merge conflict, it rebases from the gate's stale local state
// and force-pushes - discarding the origin-only commit. The lease was anchored
// to a freshly-read ls-remote SHA, which never refuses.
//
// This test reproduces the data loss at the CI auto-fix push boundary: the
// origin branch carries a commit the worktree never saw, the worktree produces
// a new head that does not contain it, and commitAndPush must REFUSE rather
// than overwrite it.
func TestCIStep_CommitAndPush_RefusesToClobberUnseenUpstreamCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature") // origin feature == H1, what no-mistakes last saw

	// Out-of-band: a reviewed commit is pushed to origin only, via a separate
	// clone, so the gate worktree never sees it.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "approved.txt"), []byte("approved review fix"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "approved review fix")
	approvedSHA := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature") // origin feature == H2 (has approved.txt)

	// The CI auto-fix agent produces a new head in the worktree that does NOT
	// contain the approved commit (simulating a rebase from stale local state).
	os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("ci fix"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = headSHA // gate's last-recorded head == H1

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)

	// The push must be refused: origin has a commit the worktree never saw.
	if err == nil {
		t.Fatalf("expected commitAndPush to refuse the divergent force-push, got pushed=%v err=nil", pushed)
	}
	if pushed {
		t.Fatalf("expected no push when refusing, got pushed=true")
	}

	// The approved commit must still be on origin.
	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != approvedSHA {
		t.Fatalf("origin feature SHA = %s, want %s (approved commit must be preserved)", originSHA, approvedSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "approved.txt") {
		t.Fatalf("approved.txt was discarded from origin - data loss")
	}
}

// Issue #305: no-mistakes rebased onto a stale view of upstream and then the
// push step force-pushed the result over an origin head that had advanced
// out-of-band, dropping the commits that landed upstream in the meantime. The
// push step must refuse to force-push over commits it never incorporated.
func TestPushStep_RefusesToClobberAdvancedUpstreamBranch(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	gitCmd(t, dir, "push", "origin", "feature") // last-seen origin == H1, tracking ref set

	// Out-of-band push advances origin/feature with work the worktree never saw.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "upstream.txt"), []byte("landed upstream"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "landed upstream")
	advancedSHA := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature") // origin == H2 (has upstream.txt)

	// The worktree's validated/rebased head does not contain the upstream commit.
	os.WriteFile(filepath.Join(dir, "validated.txt"), []byte("validated"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "validated change")
	h3 := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, h3, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = h3
	recordReviewApproval(t, sctx, h3)

	step := &PushStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatalf("expected push to refuse clobbering advanced upstream branch")
	}

	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != advancedSHA {
		t.Fatalf("origin feature SHA = %s, want %s (upstream commit must be preserved)", originSHA, advancedSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "upstream.txt") {
		t.Fatalf("upstream.txt was discarded from origin - data loss")
	}
}

// robots-1o2m regression: the CI fix agent replaced the branch instead of
// committing on top of it. It reset the worktree to the base branch and
// committed a small rewrite, so the new head's only parent was the base branch
// tip and every commit the author submitted - plus the pipeline's own document
// commit - was gone. resolveForcePushDecision waved it through because the
// remote still pointed at the head the run last pushed (the `current ==
// lastSeenSHA` fast path): that check only defends against commits that reached
// the remote OUT OF BAND, never against the pipeline dropping its own submitted
// work. The PR then looked green while fixing nothing.
//
// commitAndPush must refuse a head that does not carry the submitted head's
// changes, and leave the remote untouched.
func TestCIStep_CommitAndPush_RefusesToDropTheSubmittedHeadsWork(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// The author's submitted work: the guard plus the regression test proving it.
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "guard.go"), []byte("the fix"), 0o644)
	os.WriteFile(filepath.Join(dir, "guard_test.go"), []byte("proves the bug"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "fix(spawn): keep the guard")
	submittedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// The pipeline's own document commit lands on top and is pushed, so the
	// remote head is exactly what the run last recorded.
	os.WriteFile(filepath.Join(dir, "docs.md"), []byte("documented"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): record the guard")
	pipelineSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// CI comes back red and the fix agent replaces the branch: reset to the base
	// branch, then a small edit that reintroduces the hole the author closed.
	gitCmd(t, dir, "reset", "--hard", baseSHA)
	os.WriteFile(filepath.Join(dir, "guard.go"), []byte("skip when absent"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, submittedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = pipelineSHA // what the pipeline last pushed == remote head

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err == nil {
		t.Fatalf("expected commitAndPush to refuse a head that drops the submitted work, got pushed=%v err=nil", pushed)
	}
	if pushed {
		t.Fatalf("expected no push when refusing, got pushed=true")
	}

	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != pipelineSHA {
		t.Fatalf("origin feature SHA = %s, want %s (the branch must not be replaced)", originSHA, pipelineSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "guard_test.go") {
		t.Fatalf("the author's regression test was discarded from origin - data loss")
	}
}

// The same incident dropped the pipeline's own document commit, not just the
// author's. The gate rule keeps every pipeline fix commit present too, so a head
// that preserves the submitted work but squashes away a pipeline commit is
// refused on the runs.head_sha anchor rather than the submitted-head one.
func TestCIStep_CommitAndPush_RefusesToDropAPipelineFixCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "guard.go"), []byte("the fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "fix(spawn): keep the guard")
	submittedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	os.WriteFile(filepath.Join(dir, "docs.md"), []byte("documented"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): record the guard")
	pipelineSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// The fix agent rewinds to the submitted head - keeping the author's work but
	// dropping the pipeline's document commit - and edits from there.
	gitCmd(t, dir, "reset", "--hard", submittedSHA)
	os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("cross-platform"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, submittedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = pipelineSHA

	pushed, err := (&CIStep{}).commitAndPush(sctx)
	if err == nil {
		t.Fatalf("expected commitAndPush to refuse a head that drops a pipeline fix commit, got pushed=%v err=nil", pushed)
	}
	if pushed {
		t.Fatalf("expected no push when refusing, got pushed=true")
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "docs.md") {
		t.Fatalf("the pipeline's document commit was discarded from origin - data loss")
	}
}

// The merge-conflict CI fix prompt explicitly tells the agent to rebase onto the
// base branch, which rewrites every SHA on the branch. The submitted-work guard
// must recognize the replayed commits by content, or it would refuse the exact
// repair it asks for.
func TestCIStep_CommitAndPush_AllowsRebaseThatReplaysSubmittedWork(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "guard.go"), []byte("the fix"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "fix(spawn): keep the guard")
	submittedSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// A pipeline fix commit rides along, so the rebase must replay both anchors.
	os.WriteFile(filepath.Join(dir, "docs.md"), []byte("documented"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): record the guard")
	pipelineSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// main advances, so the fix agent rebases the branch onto it.
	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("landed on main"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main moves")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")
	gitCmd(t, dir, "rebase", "main")

	// ...and leaves the CI fix uncommitted for commitAndPush to pick up.
	os.WriteFile(filepath.Join(dir, "ci-fix.txt"), []byte("cross-platform"), 0o644)

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, submittedSHA, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.HeadSHA = pipelineSHA // what the pipeline last pushed == remote head

	step := &CIStep{}
	pushed, err := step.commitAndPush(sctx)
	if err != nil {
		t.Fatalf("expected a rebase that replays the submitted work to push, got: %v", err)
	}
	if !pushed {
		t.Fatalf("expected pushed=true after a legitimate rebase + CI fix")
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "guard.go") {
		t.Fatalf("the submitted work is missing from origin after the rebase")
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "docs.md") {
		t.Fatalf("the pipeline's own fix commit is missing from origin after the rebase")
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "ci-fix.txt") {
		t.Fatalf("the CI fix is missing from origin")
	}
}

// review-1 regression: a force-push run must not clobber an out-of-band commit
// on the PR branch. The hazard is the lease fast-path: if the rebase step
// refreshes origin/<branch> on a force push, the push step's lastSeen anchor
// equals the live remote head, so resolveForcePushDecision accepts
// `current == lastSeen` WITHOUT the patch-id content check and overwrites the
// out-of-band commit. The fix is twofold: the rebase step leaves the
// remote-tracking ref stale on a force push (so lastSeen stays the last
// *observed* head), and the content check excludes only history reachable from
// the run base. This exercises rebase + push together, the way the daemon runs
// them, so it covers the fast-path the unit tests cannot.
func TestForcePushRun_RefusesToClobberOutOfBandBranchCommit(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	m0 := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	// Feature branch v1 (M0 + A), pushed to origin. This is the gate's last
	// observed branch head and sets the local origin/feature tracking ref.
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("v1"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature v1")
	h1 := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	// Out-of-band: a reviewed commit reaches origin/feature only.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "other")
	gitCmd(t, other, "config", "user.email", "other@test.com")
	gitCmd(t, other, "checkout", "feature")
	os.WriteFile(filepath.Join(other, "approved.txt"), []byte("approved out-of-band fix"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "approved out-of-band fix")
	approvedSHA := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature") // origin/feature == H1 + C

	// The user force-pushes a rewrite of feature (drops A, adds A') that does NOT
	// contain the out-of-band commit. This is a force push relative to BaseSHA=H1.
	gitCmd(t, dir, "reset", "--hard", m0)
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("v2 rewritten"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature v2 (rewrite)")
	h1prime := gitCmd(t, dir, "rev-parse", "HEAD")

	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, h1, h1prime, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"

	// Rebase step runs first (force push detected). It must NOT refresh the
	// origin/feature tracking ref, so the push step still anchors to H1.
	rebaseOutcome, err := (&RebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rebase step: %v", err)
	}
	if rebaseOutcome != nil && rebaseOutcome.NeedsApproval {
		t.Fatalf("unexpected approval from rebase on a clean force push: %s", rebaseOutcome.Findings)
	}
	recordReviewApproval(t, sctx, gitCmd(t, dir, "rev-parse", "HEAD"))

	// Push step must refuse: origin/feature carries a commit the rewrite dropped.
	_, err = (&PushStep{}).Execute(sctx)
	if err == nil {
		t.Fatalf("expected push to refuse clobbering the out-of-band commit on a force-push run")
	}

	originSHA := gitCmd(t, upstream, "rev-parse", "refs/heads/feature")
	if originSHA != approvedSHA {
		t.Fatalf("origin feature SHA = %s, want %s (out-of-band commit must be preserved)", originSHA, approvedSHA)
	}
	if !fileAtRef(t, upstream, "refs/heads/feature", "approved.txt") {
		t.Fatalf("approved.txt was discarded from origin on a force-push run - data loss")
	}
}
