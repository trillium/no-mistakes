package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestCIStep_MergeConflictDetected_ReturnsNeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// All CI checks pass, but PR has merge conflicts
	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	ag := &mockAgent{name: "test"}
	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 0} // disabled

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			return nil
		},
	}
	useSimulatedCIClock(t, step)
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected outcome, got error: %v", err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected NeedsApproval when merge conflict detected")
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}

	foundConflict := false
	for _, f := range findings.Items {
		if strings.Contains(f.Description, "merge conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected merge conflict finding, got: %+v", findings.Items)
	}
}

func TestCIStep_MergeConflictAndCIFailure_FixPromptIncludesBoth(t *testing.T) {
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
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	sctx.Log = func(s string) {}

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	useSimulatedCIClock(t, step)
	step.Execute(sctx)

	if capturedPrompt == "" {
		t.Fatal("expected agent to be called")
	}
	if !strings.Contains(capturedPrompt, "merge conflict") {
		t.Errorf("expected prompt to mention merge conflict, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "test") {
		t.Errorf("expected prompt to mention failing check name, got:\n%s", capturedPrompt)
	}
}

func TestCIStep_MergeConflictOnly_AutoFix(t *testing.T) {
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
	gitCmd(t, dir, "push", "origin", "feature")

	// All checks pass, but merge conflict
	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"test","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	agentCalled := false
	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			agentCalled = true
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "conflict-fix.txt"), []byte("resolved"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	useSimulatedCIClock(t, step)
	step.Execute(sctx)

	if !agentCalled {
		t.Fatal("expected agent to be called to resolve merge conflict")
	}
	if strings.Contains(capturedPrompt, "You MUST produce file changes that fix the failing checks") {
		t.Fatalf("merge-conflict-only prompt should not require file changes for failing checks, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "Rebase onto the base branch and resolve the merge conflicts") {
		t.Fatalf("expected merge-conflict-only prompt to focus on rebase flow, got:\n%s", capturedPrompt)
	}

	// Should log about merge conflict
	foundConflict := false
	for _, l := range logs {
		if strings.Contains(l, "merge conflict") {
			foundConflict = true
			break
		}
	}
	if !foundConflict {
		t.Fatalf("expected merge conflict log, got: %v", logs)
	}
}

// robots-1o2m: the CI fix prompt must state the branch-continuity boundary in
// every mode. The agent that replaced the branch was told to fix failing checks
// with no word about the commits already on it, so "commit on top" was left to
// inference.
//
// This is deliberately a prompt contract, not a behavior test: the prompt cannot
// enforce anything, so asserting its text is the only thing to assert. The
// enforcement it mirrors is covered behaviorally by
// TestCIStep_CommitAndPush_RefusesToDropTheSubmittedHeadsWork,
// _RefusesToDropAPipelineFixCommit, and _AllowsRebaseThatReplaysSubmittedWork.
// The prompt exists so an agent does not burn a fix round discovering the
// refusal; drop it and the guard still holds, just later and more expensively.
func TestCIStep_AutoFixPromptForbidsReplacingTheBranch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		failingNames  []string
		mergeConflict bool
	}{
		{name: "failing_checks", failingNames: []string{"build"}},
		{name: "merge_conflict", mergeConflict: true},
		{name: "both", failingNames: []string{"build"}, mergeConflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)

			var capturedPrompt string
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					capturedPrompt = opts.Prompt
					return &agent.Result{}, nil
				},
			}

			prURL := "https://github.com/test/repo/pull/42"
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"build","state":"FAILURE","bucket":"fail"}]`)
			sctx.Run.PRURL = &prURL
			sctx.Run.HeadSHA = headSHA
			sctx.Repo.DefaultBranch = "main"

			host, skip := buildHost(sctx, scm.ProviderGitHub)
			if host == nil {
				t.Fatalf("buildHost returned nil: %s", skip)
			}
			if _, err := (&CIStep{}).autoFixCI(sctx, host, &scm.PR{Number: "42", URL: prURL}, tc.failingNames, tc.mergeConflict); err != nil {
				t.Fatalf("auto-fix CI: %v", err)
			}
			if !strings.Contains(capturedPrompt, "Commit your fixes ON TOP of the current branch head") {
				t.Fatalf("CI fix prompt does not require committing on top of the branch head:\n%s", capturedPrompt)
			}
			if !strings.Contains(capturedPrompt, "Never reset, drop, squash away, or replace commits that are already on this branch") {
				t.Fatalf("CI fix prompt does not forbid replacing existing commits:\n%s", capturedPrompt)
			}
			if !strings.Contains(capturedPrompt, "say so in your summary and leave it in place") {
				t.Fatalf("CI fix prompt does not route disagreement to a report instead of a rewrite:\n%s", capturedPrompt)
			}
		})
	}
}

func TestCIStep_MergeConflictAutoFixPromptUsesBaseBranchTip(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	featureHead := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "shared.txt")
	gitCmd(t, dir, "commit", "-m", "main update")
	mainTip := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	checksJSON := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			if err := os.WriteFile(filepath.Join(opts.CWD, "conflict-fix.txt"), []byte("resolved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, featureHead, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.DefaultBranch = "main"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 1}

	step := &CIStep{}
	useSimulatedCIClock(t, step)
	host, skip := buildHost(sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	pr := &scm.PR{Number: "42", URL: prURL}
	_, err := step.autoFixCI(sctx, host, pr, nil, true)
	if err != nil {
		t.Fatalf("auto-fix CI: %v", err)
	}
	if capturedPrompt == "" {
		t.Fatal("expected agent to receive a prompt")
	}
	if !strings.Contains(capturedPrompt, "base commit: "+mainTip) {
		t.Fatalf("expected prompt to use base branch tip %s, got:\n%s", mainTip, capturedPrompt)
	}
	if strings.Contains(capturedPrompt, "base commit: "+baseSHA) {
		t.Fatalf("expected prompt to avoid merge-base %s, got:\n%s", baseSHA, capturedPrompt)
	}
}
