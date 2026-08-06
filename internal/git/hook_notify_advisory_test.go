package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// newAdvisoryHookRepo builds a bare gate plus a work clone whose post-receive
// hook is generated against fakeScript, and returns the combined `git push`
// output the client would see.
func pushWithFakeNotify(t *testing.T, fakeScript string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("post-receive hook is /bin/sh-only")
	}
	// Every git child here is bounded: a hung one would otherwise block the
	// whole test binary rather than fail this test.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	base := t.TempDir()
	bare := filepath.Join(base, "test.git")
	if err := InitBare(ctx, bare); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	for _, args := range [][]string{
		{"init", work},
		{"-C", work, "config", "user.email", "t@t.com"},
		{"-C", work, "config", "user.name", "T"},
		{"-C", work, "remote", "add", "gate", bare},
		{"-C", work, "commit", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	fakeBin := filepath.Join(base, "fake-no-mistakes")
	if err := os.WriteFile(fakeBin, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(bare, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "post-receive"), []byte(postReceiveHookScript(fakeBin)), 0o755); err != nil {
		t.Fatal(err)
	}

	out, _ := exec.CommandContext(ctx, "git", "-C", work, "push", "gate", "HEAD:refs/heads/main").CombinedOutput()
	return string(out)
}

// notify-push exits 0 with an advisory when the daemon accepted the push but
// never confirmed the run. The hook must forward that advisory to the pushing
// client instead of swallowing it as ordinary success output, and must not also
// print the "Pipeline started" banner, which would claim more than is known
// (robots-8bao).
func TestPostReceiveHook_SurfacesUnconfirmedNotifyAdvisory(t *testing.T) {
	out := pushWithFakeNotify(t, "#!/bin/sh\necho 'TESTMARKER not a failed push: no-mistakes axi status'\nexit 0\n")

	if !strings.Contains(out, "TESTMARKER not a failed push") {
		t.Errorf("push output should surface the unconfirmed advisory, got:\n%s", out)
	}
	if strings.Contains(out, "Pipeline started") {
		t.Errorf("hook printed the started banner alongside an unconfirmed advisory:\n%s", out)
	}
	if strings.Contains(out, "notify-push failed") {
		t.Errorf("an exit-0 advisory must not be reported as a failure:\n%s", out)
	}
}

// A silent, confirmed notification keeps the existing banner.
func TestPostReceiveHook_ConfirmedNotifyKeepsBanner(t *testing.T) {
	out := pushWithFakeNotify(t, "#!/bin/sh\nexit 0\n")

	if !strings.Contains(out, "Pipeline started") {
		t.Errorf("confirmed notify-push should still print the started banner, got:\n%s", out)
	}
}
