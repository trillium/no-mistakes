package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Each native adapter must render the per-run model onto its own CLI's real
// model flag, not a guessed one.
func TestPerHarnessModelFlagMapping(t *testing.T) {
	t.Run("claude uses --model", func(t *testing.T) {
		a := &claudeAgent{bin: "claude", model: "opus"}
		assertFlagValue(t, a.buildArgs(nil, ""), "--model", "opus")
	})
	t.Run("codex uses -m", func(t *testing.T) {
		a := &codexAgent{bin: "codex", model: "gpt-5.4"}
		assertFlagValue(t, a.buildArgs("prompt", "", ""), "-m", "gpt-5.4")
	})
	t.Run("pi uses --model", func(t *testing.T) {
		a := &piAgent{bin: "pi", model: "anthropic/claude-opus-4-5"}
		assertFlagValue(t, a.buildArgs(), "--model", "anthropic/claude-opus-4-5")
	})
	t.Run("copilot uses --model", func(t *testing.T) {
		a := &copilotAgent{bin: "copilot", model: "gpt-5.4"}
		assertFlagValue(t, a.buildArgs("prompt"), "--model", "gpt-5.4")
	})
}

// No model selected means no model flag: an ordinary run's argv is unchanged.
func TestNoModelLeavesArgvUnchanged(t *testing.T) {
	if args := (&claudeAgent{bin: "claude"}).buildArgs(nil, ""); hasFlag(args, "--model") {
		t.Fatalf("claude argv gained --model without a selector model: %v", args)
	}
	if args := (&codexAgent{bin: "codex"}).buildArgs("p", "", ""); hasFlag(args, "-m") {
		t.Fatalf("codex argv gained -m without a selector model: %v", args)
	}
	if args := (&piAgent{bin: "pi"}).buildArgs(); hasFlag(args, "--model") {
		t.Fatalf("pi argv gained --model without a selector model: %v", args)
	}
	if args := (&copilotAgent{bin: "copilot"}).buildArgs("p"); hasFlag(args, "--model") {
		t.Fatalf("copilot argv gained --model without a selector model: %v", args)
	}
}

// PRECEDENCE: an explicit per-run selector model wins over agent_args_override.
// The competing user flag is removed rather than shadowed, so the outcome does
// not depend on any CLI's duplicate-flag rule.
func TestSelectorModelWinsOverAgentArgsOverride(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		a := &claudeAgent{bin: "claude", extraArgs: []string{"--model", "sonnet", "--verbose"}, model: "opus"}
		args := a.buildArgs(nil, "")
		assertFlagValue(t, args, "--model", "opus")
		assertNoValue(t, args, "sonnet")
	})
	t.Run("claude joined form", func(t *testing.T) {
		a := &claudeAgent{bin: "claude", extraArgs: []string{"--model=sonnet"}, model: "opus"}
		args := a.buildArgs(nil, "")
		assertFlagValue(t, args, "--model", "opus")
		assertNoValue(t, args, "--model=sonnet")
	})
	t.Run("codex short and long forms", func(t *testing.T) {
		a := &codexAgent{bin: "codex", extraArgs: []string{"-m", "gpt-5.2", "--model", "gpt-5.3", "--sandbox", "x"}, model: "gpt-5.4"}
		args := a.buildArgs("prompt", "", "")
		assertFlagValue(t, args, "-m", "gpt-5.4")
		assertNoValue(t, args, "gpt-5.2")
		assertNoValue(t, args, "gpt-5.3")
		if !hasFlag(args, "--sandbox") {
			t.Fatalf("unrelated user flags were dropped: %v", args)
		}
	})
	t.Run("copilot", func(t *testing.T) {
		a := &copilotAgent{bin: "copilot", extraArgs: []string{"--model", "gpt-5.2"}, model: "gpt-5.4"}
		args := a.buildArgs("prompt")
		assertFlagValue(t, args, "--model", "gpt-5.4")
		assertNoValue(t, args, "gpt-5.2")
	})
	t.Run("pi drops --provider only for a provider-qualified model", func(t *testing.T) {
		qualified := &piAgent{bin: "pi", extraArgs: []string{"--provider", "google", "--model", "gemini"}, model: "anthropic/claude-opus-4-5"}
		args := qualified.buildArgs()
		assertFlagValue(t, args, "--model", "anthropic/claude-opus-4-5")
		assertNoValue(t, args, "google")

		bare := &piAgent{bin: "pi", extraArgs: []string{"--provider", "google", "--model", "gemini-3"}, model: "gemini-3.5-flash"}
		args = bare.buildArgs()
		assertFlagValue(t, args, "--model", "gemini-3.5-flash")
		assertFlagValue(t, args, "--provider", "google")
	})
}

// The model must reach the adapter through the factory, keyed to the harness.
func TestNewWithOptionsThreadsModelToAdapters(t *testing.T) {
	claudeAg, err := NewWithOptions(types.AgentClaude, "claude", nil, Options{Model: "opus"})
	if err != nil {
		t.Fatalf("claude: %v", err)
	}
	if got := claudeAg.(*claudeAgent).model; got != "opus" {
		t.Fatalf("claude model = %q", got)
	}
	codexAg, err := NewWithOptions(types.AgentCodex, "codex", nil, Options{Model: "gpt-5.4"})
	if err != nil {
		t.Fatalf("codex: %v", err)
	}
	if got := codexAg.(*codexAgent).model; got != "gpt-5.4" {
		t.Fatalf("codex model = %q", got)
	}
	ocAg, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{Model: "github-copilot/gpt-4.1"})
	if err != nil {
		t.Fatalf("opencode: %v", err)
	}
	oc := ocAg.(*opencodeAgent)
	if oc.providerID != "github-copilot" || oc.modelID != "gpt-4.1" {
		t.Fatalf("opencode model = %q/%q, want github-copilot/gpt-4.1", oc.providerID, oc.modelID)
	}
}

// Defense in depth: even bypassing the selector grammar, a harness with no
// model channel refuses the model instead of silently ignoring it.
func TestNewWithOptionsRefusesModelOnHarnessesWithoutOne(t *testing.T) {
	for _, name := range []types.AgentName{types.AgentRovoDev, types.AgentCursor, types.AgentName("acp:gemini"), types.AgentAuto} {
		_, err := NewWithOptions(name, "bin", nil, Options{Model: "gpt-5.4"})
		if err == nil {
			t.Fatalf("NewWithOptions(%q, model) unexpectedly succeeded", name)
		}
		if !strings.Contains(err.Error(), string(name)) {
			t.Fatalf("error for %q does not name the harness: %v", name, err)
		}
	}
	// An opencode model that is not <provider>/<model> cannot be expressed.
	if _, err := NewWithOptions(types.AgentOpenCode, "opencode", nil, Options{Model: "gpt-4.1"}); err == nil {
		t.Fatal("opencode accepted a model without a provider")
	}
}

// ACP selectors keep building the untouched acpx adapter.
func TestNewWithOptionsACPPathUnchanged(t *testing.T) {
	ag, err := NewWithOptions(types.AgentName("acp:gemini"), "acpx", nil, Options{})
	if err != nil {
		t.Fatalf("acp: %v", err)
	}
	acp, ok := ag.(*acpxAgent)
	if !ok {
		t.Fatalf("acp selector built %T, want *acpxAgent", ag)
	}
	if acp.target != "gemini" {
		t.Fatalf("acp target = %q, want gemini", acp.target)
	}
}

// opencode is driven over HTTP, so its model is a request-body field. The shape
// is opencode's own: {"model":{"providerID":..,"modelID":..}}, verified against
// the running server's OpenAPI document.
func TestOpencodeMessageBodyCarriesModel(t *testing.T) {
	body := opencodeMessageBody(&opencodeAgent{providerID: "github-copilot", modelID: "gpt-4.1"}, "hello", nil)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Model *struct {
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Model == nil {
		t.Fatalf("message body carries no model: %s", raw)
	}
	if decoded.Model.ProviderID != "github-copilot" || decoded.Model.ModelID != "gpt-4.1" {
		t.Fatalf("model = %+v", *decoded.Model)
	}

	// With no per-run model the field is absent entirely, so opencode keeps
	// using its own configured default (pre-existing behavior).
	plain := opencodeMessageBody(&opencodeAgent{}, "hello", nil)
	if _, ok := plain["model"]; ok {
		t.Fatalf("message body carries a model without a selector model: %+v", plain)
	}
}

func TestDropModelArgs(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		flags []string
		want  []string
	}{
		{"separated", []string{"--model", "sonnet", "--verbose"}, []string{"--model"}, []string{"--verbose"}},
		{"joined", []string{"--model=sonnet", "--verbose"}, []string{"--model"}, []string{"--verbose"}},
		{"short", []string{"-m", "gpt-5.2", "-s", "x"}, []string{"-m", "--model"}, []string{"-s", "x"}},
		{"absent", []string{"--verbose"}, []string{"--model"}, []string{"--verbose"}},
		{"only flag", []string{"--model", "sonnet"}, []string{"--model"}, nil},
		{"trailing flag with no value", []string{"--verbose", "--model"}, []string{"--model"}, []string{"--verbose"}},
		{"empty", nil, []string{"--model"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dropModelArgs(tc.args, tc.flags); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("dropModelArgs(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, arg := range args {
		if arg == flag {
			if i+1 >= len(args) || args[i+1] != want {
				t.Fatalf("flag %s value = %v, want %q (argv %v)", flag, args[i+1:], want, args)
			}
			return
		}
	}
	t.Fatalf("flag %s missing from argv %v", flag, args)
}

func assertNoValue(t *testing.T, args []string, unwanted string) {
	t.Helper()
	for _, arg := range args {
		if arg == unwanted {
			t.Fatalf("argv still carries %q: %v", unwanted, args)
		}
	}
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
