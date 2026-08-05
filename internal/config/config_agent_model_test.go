package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func found(_ string) func(string) (string, error) {
	return func(bin string) (string, error) { return "/usr/local/bin/" + bin, nil }
}

// A <harness>:<model> selector resolves to the BARE harness plus a model kept
// beside it, so everything downstream (binary lookup, adapter construction)
// keeps seeing exactly the names it saw before.
func TestResolveAgent_SplitsHarnessAndModel(t *testing.T) {
	cases := []struct {
		selector string
		harness  types.AgentName
		model    string
	}{
		{"claude:opus", types.AgentClaude, "opus"},
		{"codex:gpt-5.4", types.AgentCodex, "gpt-5.4"},
		{"opencode:github-copilot/gpt-4.1", types.AgentOpenCode, "github-copilot/gpt-4.1"},
		{"claude", types.AgentClaude, ""},
	}
	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			cfg := &Config{Agent: types.AgentName(tc.selector)}
			if err := cfg.ResolveAgent(context.Background(), found(tc.selector)); err != nil {
				t.Fatalf("ResolveAgent: %v", err)
			}
			if cfg.Agent != tc.harness {
				t.Fatalf("agent = %q, want the bare harness %q", cfg.Agent, tc.harness)
			}
			if got := cfg.AgentModelFor(tc.harness); got != tc.model {
				t.Fatalf("AgentModelFor(%q) = %q, want %q", tc.harness, got, tc.model)
			}
			// The binary must be looked up by the harness, never the selector.
			if got := cfg.AgentPathFor(cfg.Agent); got != string(tc.harness) {
				t.Fatalf("AgentPathFor = %q, want %q", got, tc.harness)
			}
		})
	}
}

// acp:<target> must survive resolution whole: it is a bridge target, not a
// model, and splitting it would send "acp" to the native adapter switch.
func TestResolveAgent_ACPSelectorIsNotSplit(t *testing.T) {
	cfg := &Config{Agent: "acp:gemini"}
	if err := cfg.ResolveAgent(context.Background(), found("acpx")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if cfg.Agent != "acp:gemini" {
		t.Fatalf("agent = %q, want acp:gemini", cfg.Agent)
	}
	if got := cfg.AgentModelFor(cfg.Agent); got != "" {
		t.Fatalf("acp selector carried a model %q", got)
	}
	if len(cfg.AgentModels) != 0 {
		t.Fatalf("acp selector populated AgentModels: %v", cfg.AgentModels)
	}
}

// The ordered fallback list keeps working, with and without models.
func TestResolveAgent_OrderedListWithModels(t *testing.T) {
	t.Run("plain list still parses", func(t *testing.T) {
		cfg := &Config{Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}}
		if err := cfg.ResolveAgent(context.Background(), found("")); err != nil {
			t.Fatalf("ResolveAgent: %v", err)
		}
		want := []types.AgentName{types.AgentCodex, types.AgentClaude}
		if len(cfg.Agents) != len(want) || cfg.Agents[0] != want[0] || cfg.Agents[1] != want[1] {
			t.Fatalf("agents = %v, want %v", cfg.Agents, want)
		}
		if len(cfg.AgentModels) != 0 {
			t.Fatalf("plain list populated AgentModels: %v", cfg.AgentModels)
		}
	})

	t.Run("list with models", func(t *testing.T) {
		cfg := &Config{Agents: []types.AgentName{"codex:gpt-5.4", "claude:opus"}}
		if err := cfg.ResolveAgent(context.Background(), found("")); err != nil {
			t.Fatalf("ResolveAgent: %v", err)
		}
		if cfg.Agents[0] != types.AgentCodex || cfg.Agents[1] != types.AgentClaude {
			t.Fatalf("agents = %v, want [codex claude]", cfg.Agents)
		}
		if got := cfg.AgentModelFor(types.AgentCodex); got != "gpt-5.4" {
			t.Fatalf("codex model = %q", got)
		}
		if got := cfg.AgentModelFor(types.AgentClaude); got != "opus" {
			t.Fatalf("claude model = %q", got)
		}
	})

	t.Run("mixed list keeps acp whole", func(t *testing.T) {
		cfg := &Config{Agents: []types.AgentName{"claude:opus", "acp:gemini"}}
		if err := cfg.ResolveAgent(context.Background(), found("")); err != nil {
			t.Fatalf("ResolveAgent: %v", err)
		}
		if cfg.Agents[0] != types.AgentClaude || cfg.Agents[1] != "acp:gemini" {
			t.Fatalf("agents = %v, want [claude acp:gemini]", cfg.Agents)
		}
		if got := cfg.AgentModelFor(types.AgentClaude); got != "opus" {
			t.Fatalf("claude model = %q", got)
		}
	})
}

// An invalid selector fails resolution with the reason, rather than probing for
// a binary named after the whole selector.
func TestResolveAgent_InvalidSelectorFailsWithReason(t *testing.T) {
	cases := []struct{ selector, want string }{
		{"rovodev:gpt-5.4", "rovodev"},
		{"cursor:opus", "cursor"},
		{"claude:", "non-empty model"},
		{"opencode:gpt-4.1", "<provider>/<model>"},
	}
	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			cfg := &Config{Agent: types.AgentName(tc.selector)}
			err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
				return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
			})
			if err == nil {
				t.Fatalf("selector %q resolved without error", tc.selector)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// AgentModels is derived per resolution, so the config-file YAML list form
// (`agent: [codex:gpt-5.4, claude:opus]`) parses and resolves end to end.
func TestLoadGlobal_AgentListWithModels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agent:\n  - codex:gpt-5.4\n  - claude:opus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if len(cfg.Agents) != 2 || cfg.Agents[0] != "codex:gpt-5.4" || cfg.Agents[1] != "claude:opus" {
		t.Fatalf("parsed agents = %v", cfg.Agents)
	}
	merged := Merge(cfg, &RepoConfig{})
	if err := merged.ResolveAgent(context.Background(), found("")); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	if merged.AgentModelFor(types.AgentCodex) != "gpt-5.4" || merged.AgentModelFor(types.AgentClaude) != "opus" {
		t.Fatalf("models = %v", merged.AgentModels)
	}
}
