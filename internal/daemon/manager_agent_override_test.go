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
