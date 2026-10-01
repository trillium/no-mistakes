//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// TestLoginShellPathSeedShadowsSystemToolsForTheDaemon pins the invariant the
// whole offline e2e suite rests on: the daemon does NOT run steps with the
// PATH it inherits from `go test`. It re-resolves its environment from a
// LOGIN, INTERACTIVE shell (shellenv.resolveUncached), and the step executor
// resolves binaries out of that PATH. So the harness's BinDir prepend to
// os.Environ is not what decides whether `gh` is the fakeagent stub or the
// developer's authenticated /opt/homebrew/bin/gh - the login shell's own PATH
// ordering is.
//
// Seeding only .zshenv/.zprofile/.bash_profile/.profile was not enough on
// macOS: an interactive zsh reads /etc/zshrc after ~/.zprofile, and the
// Homebrew prefix lands ahead of the seeded BinDir there. The PR step then
// reached the real GitHub API with the fixtures' nonexistent repo slugs and
// three tests failed only on a developer machine with an authenticated gh,
// never in CI. The seed therefore has to cover every startup file the probe
// shell can read, with the last-read file re-winning the prepend.
func TestLoginShellPathSeedShadowsSystemToolsForTheDaemon(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	stub := filepath.Join(binDir, "gh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	writeLoginShellPathSeed(t, home, binDir)

	path := probeLoginShellPath(t, home)
	got := firstOnPath(path, "gh")
	if got != stub {
		t.Fatalf("login-shell PATH resolves gh to %q, want the harness stub %q\nPATH=%s", got, stub, path)
	}
}

// probeLoginShellPath resolves PATH exactly the way the daemon does at
// startup: the same shell, the same login/interactive flags, the same
// `env -0` output shape as shellenv.resolveUncached.
func probeLoginShellPath(t *testing.T, home string) string {
	t.Helper()

	shell := shellenv.LoginShell()
	args := []string{"-l", "-c", "env -0"}
	if shellenv.SupportsInteractive(shell) {
		args = []string{"-l", "-i", "-c", "env -0"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe login shell %s %v: %v", shell, args, err)
	}

	for _, entry := range strings.Split(string(out), "\x00") {
		entry = strings.TrimLeft(entry, "\r\n")
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			return value
		}
	}
	t.Fatalf("login shell %s reported no PATH entry", shell)
	return ""
}

// firstOnPath returns the absolute path of the first executable named name
// found by walking path in order, or "" when nothing matches.
func firstOnPath(path, name string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}
