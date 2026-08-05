package daemon

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestApplyRunAgentOverride(t *testing.T) {
	t.Run("override replaces configured agent and single-agent list", func(t *testing.T) {
		cfg := &config.Config{
			Agent:  types.AgentClaude,
			Agents: []types.AgentName{types.AgentClaude, types.AgentCodex},
		}
		applyRunAgentOverride(cfg, "  codex  ")
		if cfg.Agent != types.AgentCodex {
			t.Fatalf("Agent = %q, want %q", cfg.Agent, types.AgentCodex)
		}
		if len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
			t.Fatalf("Agents = %v, want [codex]", cfg.Agents)
		}
	})

	t.Run("blank override leaves the configured agent untouched", func(t *testing.T) {
		cfg := &config.Config{
			Agent:  types.AgentClaude,
			Agents: []types.AgentName{types.AgentClaude},
		}
		applyRunAgentOverride(cfg, "   ")
		if cfg.Agent != types.AgentClaude {
			t.Fatalf("Agent = %q, want unchanged %q", cfg.Agent, types.AgentClaude)
		}
		if len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentClaude {
			t.Fatalf("Agents = %v, want unchanged [claude]", cfg.Agents)
		}
	})
}

// A per-run override carrying a model is persisted verbatim in
// runs.agent_override and replayed through the same path on recovery, so a
// mid-run daemon restart rebuilds the SAME model, not just the same harness.
func TestApplyRunAgentOverride_ModelSurvivesRecovery(t *testing.T) {
	cases := []struct {
		override string
		harness  types.AgentName
		model    string
	}{
		{"opencode:github-copilot/gpt-4.1", types.AgentOpenCode, "github-copilot/gpt-4.1"},
		{"claude:opus", types.AgentClaude, "opus"},
		{"  codex:gpt-5.4  ", types.AgentCodex, "gpt-5.4"},
		{"acp:gemini", "acp:gemini", ""},
		{"codex", types.AgentCodex, ""},
	}
	for _, tc := range cases {
		t.Run(tc.override, func(t *testing.T) {
			// The recovery path (prepareRecoveredRun) applies the persisted
			// override to a freshly loaded config and then resolves it.
			cfg := &config.Config{Agent: types.AgentClaude}
			applyRunAgentOverride(cfg, tc.override)
			if err := cfg.ResolveAgent(t.Context(), func(bin string) (string, error) {
				return "/usr/local/bin/" + bin, nil
			}); err != nil {
				t.Fatalf("ResolveAgent: %v", err)
			}
			if cfg.Agent != tc.harness {
				t.Fatalf("recovered harness = %q, want %q", cfg.Agent, tc.harness)
			}
			if got := cfg.AgentModelFor(tc.harness); got != tc.model {
				t.Fatalf("recovered model = %q, want %q", got, tc.model)
			}
		})
	}
}
