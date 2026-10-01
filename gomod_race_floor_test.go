package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// raceSafeGoFloor is the first Go release on the 1.26 line carrying the fix for
// golang/go#78059. See TestGoMod_RaceSafeToolchainFloor for why it is a floor.
var raceSafeGoFloor = [3]int{1, 26, 5}

var goDirectiveRE = regexp.MustCompile(`(?m)^go (\d+)\.(\d+)(?:\.(\d+))?$`)

// The whole -race suite depends on this floor. golang/go#78059 let
// ThreadSanitizer use uninitialized memory on a thread that emits no TSAN
// events; the corrupted state then killed the child of the next fork/exec,
// between fork() and execve(). This repository forks constantly under test, so
// the symptom was a segfault in a random exec-ing test that always passed in
// isolation - it reads as an order-dependent flake and cost two rounds of
// edits to innocent tests before it was attributed.
//
// The floor must live on the `go` directive, not a `toolchain` line: Go selects
// the max of the local and declared toolchains, and the broken 1.26.4 sorts
// above the fixed 1.25.12, so a toolchain line cannot exclude 1.26.0-1.26.4.
// Only a `go` directive at or above 1.26.5 rules out every affected release.
//
// Lowering this floor silently reopens the flake, so pin it here.
func TestGoMod_RaceSafeToolchainFloor(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	content := string(raw)

	m := goDirectiveRE.FindStringSubmatch(content)
	if m == nil {
		t.Fatal("go.mod has no parseable `go` directive")
	}
	var got [3]int
	for i := 0; i < 3; i++ {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			t.Fatalf("parse go directive %q: %v", m[0], err)
		}
		got[i] = n
	}

	for i := 0; i < 3; i++ {
		if got[i] > raceSafeGoFloor[i] {
			break
		}
		if got[i] < raceSafeGoFloor[i] {
			t.Fatalf("go.mod declares %q, but the -race suite requires Go >= %d.%d.%d: "+
				"every earlier release is affected by golang/go#78059, where TSAN corruption "+
				"kills the child of any fork/exec and surfaces as a segfault in a random "+
				"exec-ing test (see the AGENTS.md Testing Conventions entry)",
				strings.TrimSpace(m[0]), raceSafeGoFloor[0], raceSafeGoFloor[1], raceSafeGoFloor[2])
		}
	}

	// A toolchain line cannot enforce the floor (Go takes the max of local and
	// declared), so one below the floor is a false sense of safety.
	if tm := regexp.MustCompile(`(?m)^toolchain go(\d+)\.(\d+)(?:\.(\d+))?$`).FindStringSubmatch(content); tm != nil {
		t.Errorf("go.mod declares %q; a toolchain line cannot enforce the golang/go#78059 floor "+
			"because Go selects the max of the local and declared toolchains - keep the floor on the `go` directive",
			strings.TrimSpace(tm[0]))
	}
}
