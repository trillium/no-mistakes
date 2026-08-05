package types

import "testing"

func TestParseAgentSelector_BareHarnessesKeepWorking(t *testing.T) {
	for _, name := range []string{"auto", "claude", "codex", "rovodev", "opencode", "pi", "copilot", "cursor"} {
		sel, err := ParseAgentSelector(name)
		if err != nil {
			t.Fatalf("ParseAgentSelector(%q) error: %v", name, err)
		}
		if sel.Harness != AgentName(name) {
			t.Fatalf("ParseAgentSelector(%q) harness = %q, want %q", name, sel.Harness, name)
		}
		if sel.Model != "" {
			t.Fatalf("ParseAgentSelector(%q) model = %q, want empty", name, sel.Model)
		}
	}
}

// The whole point of the reserved prefix: acp:<target> must parse to the
// pre-existing ACP selector with NO model, never to harness "acp" + model.
func TestParseAgentSelector_ACPTargetsAreNotModels(t *testing.T) {
	for _, raw := range []string{"acp:gemini", "acp:cursor", "acp:some-target"} {
		sel, err := ParseAgentSelector(raw)
		if err != nil {
			t.Fatalf("ParseAgentSelector(%q) error: %v", raw, err)
		}
		if string(sel.Harness) != raw {
			t.Fatalf("ParseAgentSelector(%q) harness = %q, want the whole selector", raw, sel.Harness)
		}
		if sel.Model != "" {
			t.Fatalf("ParseAgentSelector(%q) model = %q, want empty (acp target is not a model)", raw, sel.Model)
		}
		target, ok := ACPTargetFor(sel.Harness)
		if !ok || target == "" {
			t.Fatalf("ParseAgentSelector(%q) harness does not resolve to an ACP target", raw)
		}
	}
}

func TestParseAgentSelector_HarnessModel(t *testing.T) {
	cases := []struct {
		raw     string
		harness AgentName
		model   string
	}{
		{"claude:opus", AgentClaude, "opus"},
		{"claude:claude-opus-4-5-20251101", AgentClaude, "claude-opus-4-5-20251101"},
		{"codex:gpt-5.4", AgentCodex, "gpt-5.4"},
		{"opencode:github-copilot/gpt-4.1", AgentOpenCode, "github-copilot/gpt-4.1"},
		{"pi:anthropic/claude-opus-4-5", AgentPi, "anthropic/claude-opus-4-5"},
		{"copilot:gpt-5.4", AgentCopilot, "gpt-5.4"},
	}
	for _, tc := range cases {
		sel, err := ParseAgentSelector(tc.raw)
		if err != nil {
			t.Fatalf("ParseAgentSelector(%q) error: %v", tc.raw, err)
		}
		if sel.Harness != tc.harness || sel.Model != tc.model {
			t.Fatalf("ParseAgentSelector(%q) = (%q, %q), want (%q, %q)", tc.raw, sel.Harness, sel.Model, tc.harness, tc.model)
		}
	}
}

// A model may contain slashes but the split is on the FIRST colon only, so a
// second colon is rejected rather than silently folded into the model.
func TestParseAgentSelector_ModelMayContainSlashButNotColon(t *testing.T) {
	sel, err := ParseAgentSelector("opencode:github-copilot/gpt-4.1")
	if err != nil {
		t.Fatalf("slash model rejected: %v", err)
	}
	if sel.Model != "github-copilot/gpt-4.1" {
		t.Fatalf("model = %q, want github-copilot/gpt-4.1", sel.Model)
	}
	if _, err := ParseAgentSelector("claude:opus:extra"); err == nil {
		t.Fatal("expected a colon inside the model to be rejected")
	}
}

func TestParseAgentSelector_Invalid(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"empty acp target", "acp:"},
		{"bare acp", "acp"},
		{"empty model", "claude:"},
		{"unknown harness", "bogus"},
		{"unknown harness with model", "bogus:gpt-5"},
		{"model on auto", "auto:opus"},
		{"model on rovodev", "rovodev:gpt-5.4"},
		{"model on cursor alias", "cursor:gpt-5.4"},
		{"opencode model without provider", "opencode:gpt-4.1"},
		{"opencode empty provider", "opencode:/gpt-4.1"},
		{"opencode empty model id", "opencode:github-copilot/"},
		{"whitespace inside", "claude: opus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if sel, err := ParseAgentSelector(tc.raw); err == nil {
				t.Fatalf("ParseAgentSelector(%q) unexpectedly succeeded: %+v", tc.raw, sel)
			}
			if ValidAgentSelector(AgentName(tc.raw)) {
				t.Fatalf("ValidAgentSelector(%q) = true, want false", tc.raw)
			}
		})
	}
}

// A harness that cannot express a model must be refused by name, not silently
// ignored - a silent drop runs the wrong model while reporting success.
func TestParseAgentSelector_UnsupportedModelErrorNamesTheHarness(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"rovodev:gpt-5.4", "rovodev"},
		{"cursor:gpt-5.4", "cursor"},
		{"auto:opus", "auto"},
	} {
		_, err := ParseAgentSelector(tc.raw)
		if err == nil {
			t.Fatalf("ParseAgentSelector(%q) unexpectedly succeeded", tc.raw)
		}
		if !contains(err.Error(), tc.want) {
			t.Fatalf("error %q does not name the harness %q", err, tc.want)
		}
	}
}

// Everything after the reserved acp: prefix is the ACP target verbatim, colons
// included. Applying the model split there would break existing acp targets.
func TestParseAgentSelector_ACPTargetKeepsItsOwnColons(t *testing.T) {
	sel, err := ParseAgentSelector("acp:gemini:pro")
	if err != nil {
		t.Fatalf("ParseAgentSelector(acp:gemini:pro) error: %v", err)
	}
	if sel.Model != "" {
		t.Fatalf("model = %q, want empty", sel.Model)
	}
	target, ok := ACPTargetFor(sel.Harness)
	if !ok || target != "gemini:pro" {
		t.Fatalf("ACP target = %q (ok=%v), want gemini:pro", target, ok)
	}
}

func TestValidAgentSelector_AcceptsModels(t *testing.T) {
	for _, raw := range []string{"claude", "acp:gemini", "claude:opus", "codex:gpt-5.4", "opencode:github-copilot/gpt-4.1"} {
		if !ValidAgentSelector(AgentName(raw)) {
			t.Fatalf("ValidAgentSelector(%q) = false, want true", raw)
		}
	}
}

func TestSplitOpenCodeModel(t *testing.T) {
	provider, model, ok := SplitOpenCodeModel("github-copilot/gpt-4.1")
	if !ok || provider != "github-copilot" || model != "gpt-4.1" {
		t.Fatalf("SplitOpenCodeModel = (%q, %q, %v)", provider, model, ok)
	}
	// A model id may itself contain slashes; only the first one is the split.
	provider, model, ok = SplitOpenCodeModel("openrouter/vendor/model-1")
	if !ok || provider != "openrouter" || model != "vendor/model-1" {
		t.Fatalf("SplitOpenCodeModel nested = (%q, %q, %v)", provider, model, ok)
	}
	for _, bad := range []string{"gpt-4.1", "/gpt-4.1", "github-copilot/", ""} {
		if _, _, ok := SplitOpenCodeModel(bad); ok {
			t.Fatalf("SplitOpenCodeModel(%q) unexpectedly ok", bad)
		}
	}
}

func TestAgentSupportsModel(t *testing.T) {
	for _, name := range []AgentName{AgentClaude, AgentCodex, AgentOpenCode, AgentPi, AgentCopilot} {
		if !AgentSupportsModel(name) {
			t.Fatalf("AgentSupportsModel(%q) = false, want true", name)
		}
	}
	for _, name := range []AgentName{AgentAuto, AgentRovoDev, AgentCursor, AgentName("acp:gemini")} {
		if AgentSupportsModel(name) {
			t.Fatalf("AgentSupportsModel(%q) = true, want false", name)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
