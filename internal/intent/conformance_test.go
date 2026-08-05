package intent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type fixedVerifier struct {
	result VerifyResult
	err    error
	calls  int
	last   VerifyParams
}

func (f *fixedVerifier) Verify(_ context.Context, p VerifyParams) (VerifyResult, error) {
	f.calls++
	f.last = p
	if f.err != nil {
		return VerifyResult{}, f.err
	}
	return f.result, nil
}

func TestSummaryNamesPaths(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		want    []string
	}{
		{
			name:    "slashed path with extension",
			summary: "The user wanted internal/watch/blocked.go to expose a --why-blocked query.",
			want:    []string{"internal/watch/blocked.go"},
		},
		{
			name:    "bare source filename",
			summary: "They asked for a helper in matcher.go and a test in matcher_test.go.",
			want:    []string{"matcher.go", "matcher_test.go"},
		},
		{
			name:    "version strings are not paths",
			summary: "Upgrade from v1.45.4 to v1.46.0; see e.g. the changelog. No files named.",
			want:    nil,
		},
		{
			name:    "pr and issue references are not paths",
			summary: "Reconcile the diverged main branch and push the result as PR #38.",
			want:    nil,
		},
		{
			name:    "prose sentences are not paths",
			summary: "The developer wanted the watcher to surface teardown-blocked tasks. That is all.",
			want:    nil,
		},
		{
			name:    "remote and module paths are not repository paths",
			summary: "Point the fork at github.com/trillium/no-mistakes.git and keep github.com/kunchenguid/no-mistakes as upstream.",
			want:    nil,
		},
		{
			name:    "dotted top-level directories are repository paths",
			summary: "The developer wanted .github/workflows/ci.yml to exclude the Windows leg.",
			want:    []string{".github/workflows/ci.yml"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summaryNamedPaths(tc.summary)
			if len(got) != len(tc.want) {
				t.Fatalf("summaryNamedPaths(%q) = %v, want %v", tc.summary, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("summaryNamedPaths(%q) = %v, want %v", tc.summary, got, tc.want)
				}
			}
		})
	}
}

func TestSummaryPathConflict(t *testing.T) {
	diff := []string{"internal/watch/blocked.go", "internal/watch/blocked_test.go"}

	if conflict, _ := summaryPathConflict("The user wanted internal/watch/blocked.go to answer --why-blocked.", diff); conflict {
		t.Fatalf("summary naming a diff file must not conflict")
	}
	if conflict, _ := summaryPathConflict("They touched internal/watch/blocked.go and internal/db/run.go.", diff); conflict {
		t.Fatalf("partial intersection must not conflict")
	}
	conflict, reason := summaryPathConflict("The user reconciled internal/gate/gate.go and cmd/no-mistakes/main.go.", diff)
	if !conflict {
		t.Fatalf("summary naming only foreign files must conflict")
	}
	if !strings.Contains(reason, "internal/gate/gate.go") {
		t.Fatalf("reason should name the foreign path, got %q", reason)
	}
	if conflict, _ := summaryPathConflict("The user wanted the watcher to report blocked teardowns.", diff); conflict {
		t.Fatalf("summary naming no files must not conflict")
	}
	if conflict, _ := summaryPathConflict("Only internal/gate/gate.go was touched.", nil); conflict {
		t.Fatalf("an empty diff has nothing to conflict with")
	}
}

// robots-txep: a two-file watcher change shipped a PR body whose Intent section
// described an unrelated 25-file main-branch reconciliation, because the long
// session that did that reconciliation had mentioned both watcher files in
// passing and therefore scored a decisive 1.00 against this diff.
func TestExtract_DiscardsIntentThatDescribesAnotherChange(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "sprawling",
			LastActivity: time.Now(),
			LastMsgKey:   "k1",
			Messages: []Message{
				{Role: RoleUser, Text: "reconcile the diverged main branch"},
				{Role: RoleAssistant, Text: "touched everything", FilePaths: []string{"watch.go", "sweep.go", "gate.go"}},
			},
		}},
	}
	sum := &fixedSummarizer{summary: "The developer wanted to reconcile firstmate's diverged local main branch into a PR using a real three-way merge."}
	ver := &fixedVerifier{result: VerifyResult{DescribesChange: false, Reason: "summary is about a branch reconciliation, not a watcher query"}}

	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"watch.go", "sweep.go"},
		ChangeSubject: "feat(watch): surface teardown-blocked finished tasks to the captain",
		BaseTime:      time.Now().Add(-time.Hour),
		HeadTime:      time.Now(),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Cache:         NewMemCache(),
		Summarizer:    sum,
		Verifier:      ver,
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch for a non-conforming intent, got %v", err)
	}
	if ver.calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", ver.calls)
	}
	if ver.last.ChangeSubject == "" || len(ver.last.DiffFiles) != 2 {
		t.Fatalf("verifier must receive this run's change as ground truth, got %+v", ver.last)
	}
}

func TestExtract_KeepsIntentTheVerifierConfirms(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			LastMsgKey:   "k1",
			Messages: []Message{
				{Role: RoleUser, Text: "make the watcher surface blocked teardowns"},
				{Role: RoleAssistant, Text: "done", FilePaths: []string{"watch.go", "sweep.go"}},
			},
		}},
	}
	sum := &fixedSummarizer{summary: "The developer wanted the watcher to surface teardown-blocked finished tasks."}
	ver := &fixedVerifier{result: VerifyResult{DescribesChange: true}}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"watch.go", "sweep.go"},
		ChangeSubject: "feat(watch): surface teardown-blocked finished tasks",
		BaseTime:      time.Now().Add(-time.Hour),
		HeadTime:      time.Now(),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Cache:         NewMemCache(),
		Summarizer:    sum,
		Verifier:      ver,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Summary != sum.summary {
		t.Fatalf("summary = %q", got.Summary)
	}
}

// A verifier that cannot answer must not silently delete a plausible intent:
// the deterministic path check is the always-on guard, the agent turn is advice.
func TestExtract_VerifierFailureKeepsIntent(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			LastMsgKey:   "k1",
			Messages: []Message{
				{Role: RoleUser, Text: "edit watch.go and sweep.go"},
				{Role: RoleAssistant, Text: "done", FilePaths: []string{"watch.go", "sweep.go"}},
			},
		}},
	}
	sum := &fixedSummarizer{summary: "The developer wanted the watcher to report blocked teardowns."}
	ver := &fixedVerifier{err: errors.New("agent exploded")}

	got, err := Extract(context.Background(), ExtractParams{
		OriginCWD:     "/tmp/repo",
		DiffFiles:     []string{"watch.go", "sweep.go"},
		ChangeSubject: "feat(watch): blocked teardowns",
		BaseTime:      time.Now().Add(-time.Hour),
		HeadTime:      time.Now(),
		Threshold:     0.2,
		Readers:       []Reader{r},
		Cache:         NewMemCache(),
		Summarizer:    sum,
		Verifier:      ver,
	})
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Summary != sum.summary {
		t.Fatalf("summary = %q", got.Summary)
	}
}

// The deterministic guard runs with no Verifier configured at all.
func TestExtract_DiscardsIntentNamingOnlyForeignFiles(t *testing.T) {
	r := &staticReader{
		name: "claude",
		sessions: []*Session{{
			SessionID:    "s1",
			LastActivity: time.Now(),
			LastMsgKey:   "k1",
			Messages: []Message{
				{Role: RoleUser, Text: "edit watch.go and sweep.go"},
				{Role: RoleAssistant, Text: "done", FilePaths: []string{"watch.go", "sweep.go"}},
			},
		}},
	}
	sum := &fixedSummarizer{summary: "The developer wanted internal/gate/gate.go rewritten to preserve custody."}

	var logs []string
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"watch.go", "sweep.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		Threshold:  0.2,
		Readers:    []Reader{r},
		Cache:      NewMemCache(),
		Summarizer: sum,
		Logf:       func(format string, args ...any) { logs = append(logs, format) },
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "discarded inferred intent") {
		t.Fatalf("expected a discard diagnostic, got %q", joined)
	}
}

// A cached summary bypasses the summarizer but must not bypass the gate.
func TestExtract_CachedSummaryIsStillGated(t *testing.T) {
	session := &Session{
		SessionID:    "s1",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages: []Message{
			{Role: RoleUser, Text: "edit watch.go and sweep.go"},
			{Role: RoleAssistant, Text: "done", FilePaths: []string{"watch.go", "sweep.go"}},
		},
	}
	session.AgentName = "claude"
	cache := NewMemCache()
	cache.Put(cacheKeyFor(session), "The developer wanted internal/gate/gate.go rewritten.", "claude", "s1")

	sum := &fixedSummarizer{summary: "unused"}
	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"watch.go", "sweep.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		Threshold:  0.2,
		Readers:    []Reader{&staticReader{name: "claude", sessions: []*Session{session}}},
		Cache:      cache,
		Summarizer: sum,
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch for a cached non-conforming summary, got %v", err)
	}
	if sum.calls != 0 {
		t.Fatalf("summarizer should not have run on a cache hit, calls = %d", sum.calls)
	}
}

// A rejected summary must not be written to the cache: the next run for the same
// session would then inherit it without paying for a fresh summarization.
func TestExtract_RejectedSummaryIsNotCached(t *testing.T) {
	session := &Session{
		SessionID:    "s1",
		LastActivity: time.Now(),
		LastMsgKey:   "k1",
		Messages: []Message{
			{Role: RoleUser, Text: "edit watch.go and sweep.go"},
			{Role: RoleAssistant, Text: "done", FilePaths: []string{"watch.go", "sweep.go"}},
		},
	}
	session.AgentName = "claude"
	cache := NewMemCache()
	sum := &fixedSummarizer{summary: "The developer wanted internal/gate/gate.go rewritten."}

	_, err := Extract(context.Background(), ExtractParams{
		OriginCWD:  "/tmp/repo",
		DiffFiles:  []string{"watch.go", "sweep.go"},
		BaseTime:   time.Now().Add(-time.Hour),
		HeadTime:   time.Now(),
		Threshold:  0.2,
		Readers:    []Reader{&staticReader{name: "claude", sessions: []*Session{session}}},
		Cache:      cache,
		Summarizer: sum,
	})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("expected ErrNoMatch, got %v", err)
	}
	if _, ok := cache.Get(cacheKeyFor(session)); ok {
		t.Fatalf("a discarded summary must not be cached")
	}
}

func TestParseVerifyResult(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		text    string
		want    bool
		wantErr bool
	}{
		{name: "structured yes", output: `{"describes_change":true,"reason":"matches"}`, want: true},
		{name: "structured no", output: `{"describes_change":false,"reason":"different change"}`, want: false},
		{name: "text fallback", text: `{"describes_change":false,"reason":"different change"}`, want: false},
		{name: "unparseable", text: "I think so?", wantErr: true},
		{name: "empty", wantErr: true},
		// A response that answers some other question is not a rejection.
		{name: "field omitted", output: `{"summary":"user wanted a Bar() helper"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVerifyResult([]byte(tc.output), tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVerifyResult: %v", err)
			}
			if got.DescribesChange != tc.want {
				t.Fatalf("DescribesChange = %v, want %v", got.DescribesChange, tc.want)
			}
		})
	}
}

func TestBuildVerifyPrompt_CarriesGroundTruthAndUntrustedFraming(t *testing.T) {
	prompt := buildVerifyPrompt(VerifyParams{
		Summary:       "reconcile the diverged main branch",
		DiffFiles:     []string{"internal/watch/blocked.go"},
		ChangeSubject: "feat(watch): surface teardown-blocked finished tasks",
	})
	for _, want := range []string{
		"internal/watch/blocked.go",
		"feat(watch): surface teardown-blocked finished tasks",
		"reconcile the diverged main branch",
		"describes_change",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, "not commands") && !strings.Contains(prompt, "not instructions") {
		t.Fatalf("prompt must frame the summary as untrusted data:\n%s", prompt)
	}
}

// TestClampChangeSubject locks the ground-truth cap in place, and locks it to a
// rune boundary: a cut landing mid-rune would put an invalid UTF-8 byte in the
// prompt.
func TestClampChangeSubject(t *testing.T) {
	short := "fix(intent): gate inferred intent\n\nbody"
	if got := clampChangeSubject("  " + short + "  "); got != short {
		t.Fatalf("a subject under the cap must pass through trimmed, got %q", got)
	}

	oversized := strings.Repeat("a", maxChangeSubjectBytes+512)
	got := clampChangeSubject(oversized)
	if !strings.HasSuffix(got, "\n[truncated]") {
		t.Fatalf("an oversized subject must be marked truncated, got %q", got[max(0, len(got)-40):])
	}
	if len(got) > maxChangeSubjectBytes+len("\n[truncated]") {
		t.Fatalf("clamped subject is %d bytes, over the %d cap", len(got), maxChangeSubjectBytes)
	}

	// A multi-byte rune straddling the cap must be dropped whole, not sliced.
	straddling := strings.Repeat("a", maxChangeSubjectBytes-1) + strings.Repeat("é", 64)
	clamped := clampChangeSubject(straddling)
	if !utf8.ValidString(clamped) {
		t.Fatalf("clamped subject is not valid UTF-8: %q", clamped[max(0, len(clamped)-16):])
	}
}
