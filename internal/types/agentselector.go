package types

import (
	"fmt"
	"strings"
)

// AgentSelector is the parsed form of a pipeline agent selector string, the one
// grammar shared by the config `agent` field and the per-run `axi run --agent`
// override.
//
// Grammar:
//
//	<harness>                  e.g. auto, claude, codex, opencode, cursor
//	acp:<target>               an ACP bridge target (RESERVED prefix, never a model)
//	<harness>:<model>          e.g. claude:opus, opencode:github-copilot/gpt-4.1
//
// The string is split on the FIRST colon only, and the prefix decides the
// meaning. "acp" is reserved: acp:<target> names an ACP bridge target and has
// nothing to do with models, so it keeps the pre-existing path untouched.
// Everything else before the first colon is a harness and everything after it
// is that harness's model. A model may contain slashes (opencode model ids are
// <provider>/<model>) but never a colon, so first-colon splitting is
// unambiguous.
type AgentSelector struct {
	// Harness is the bare selector the agent factory builds: "auto", a native
	// adapter name, a first-class ACP alias (cursor), or an explicit
	// acp:<target>. It never carries a model.
	Harness AgentName
	// Model is the per-run model for Harness, empty when the selector named
	// none. It is passed to the adapter through agent.Options.Model.
	Model string
}

// acpSelectorPrefix is the reserved selector prefix. acp:<target> is an ACP
// bridge target, NOT <harness>:<model>; the collision is resolved by branching
// on the prefix before the model split.
const acpSelectorPrefix = "acp"

// validAgentSelectorHint is the shared "what you may pass" text so the CLI
// flag, the config error, and the agent factory all name the same set.
const validAgentSelectorHint = "auto, claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target>, or <harness>:<model>"

// ValidAgentSelectorHint returns the human-readable list of accepted selector
// shapes for error messages and flag help.
func ValidAgentSelectorHint() string { return validAgentSelectorHint }

// nativeAgentNames is the set of native adapter names the agent factory can
// construct. It is the selector-name authority; adapter construction switches
// on the same names.
var nativeAgentNames = map[AgentName]bool{
	AgentClaude:   true,
	AgentCodex:    true,
	AgentRovoDev:  true,
	AgentOpenCode: true,
	AgentPi:       true,
	AgentCopilot:  true,
}

// modelUnsupportedReason explains why a harness cannot accept a per-run model.
// A harness absent from this map accepts one. Rejecting loudly is deliberate:
// silently ignoring `--agent rovodev:gpt-5.4` would run the wrong model while
// reporting success.
var modelUnsupportedReason = map[AgentName]string{
	AgentAuto: "auto picks the harness by probing, so a model cannot be mapped to a CLI flag; " +
		"name the harness explicitly (e.g. claude:opus)",
	AgentRovoDev: "the acli rovodev server exposes no per-run model selector",
}

// acpModelUnsupportedReason is used for every ACP selector (the cursor alias
// and explicit acp:<target> alike).
const acpModelUnsupportedReason = "ACP targets carry no model channel; select the model in the ACP agent's own configuration"

// ParseAgentSelector parses and validates a selector string. It does not probe
// whether any binary is installed - only that the selector is a shape the
// daemon knows how to build.
func ParseAgentSelector(raw string) (AgentSelector, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return AgentSelector{}, fmt.Errorf("agent selector must not be empty; valid options: %s", validAgentSelectorHint)
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return AgentSelector{}, fmt.Errorf("invalid agent selector %q: must not contain whitespace", raw)
	}

	head, tail, hasColon := strings.Cut(value, ":")

	// RESERVED: acp:<target> is an ACP bridge target, not a model. Branch on
	// the prefix before anything model-shaped happens so the pre-existing ACP
	// path is bit-for-bit unchanged.
	if head == acpSelectorPrefix {
		if !hasColon || tail == "" {
			return AgentSelector{}, fmt.Errorf("invalid agent selector %q: acp:<target> requires a non-empty target", raw)
		}
		name := AgentName(value)
		if _, ok := ACPTargetFor(name); !ok {
			return AgentSelector{}, fmt.Errorf("invalid agent selector %q: acp:<target> requires a non-empty target", raw)
		}
		return AgentSelector{Harness: name}, nil
	}

	harness := AgentName(head)
	if !knownBareAgentName(harness) {
		return AgentSelector{}, fmt.Errorf("unknown agent %q; valid options: %s", raw, validAgentSelectorHint)
	}
	if !hasColon {
		return AgentSelector{Harness: harness}, nil
	}

	model := tail
	if model == "" {
		return AgentSelector{}, fmt.Errorf("invalid agent selector %q: <harness>:<model> requires a non-empty model", raw)
	}
	if strings.Contains(model, ":") {
		return AgentSelector{}, fmt.Errorf(
			"invalid agent selector %q: the model %q must not contain ':' (a selector splits on its first colon, and only the acp: prefix is reserved)",
			raw, model)
	}
	if reason, unsupported := AgentModelUnsupportedReason(harness); unsupported {
		return AgentSelector{}, fmt.Errorf("agent %q does not accept a model: %s", harness, reason)
	}
	if harness == AgentOpenCode {
		if _, _, ok := SplitOpenCodeModel(model); !ok {
			return AgentSelector{}, fmt.Errorf(
				"invalid opencode model %q: opencode model ids are <provider>/<model> (e.g. github-copilot/gpt-4.1)", model)
		}
	}
	return AgentSelector{Harness: harness, Model: model}, nil
}

// knownBareAgentName reports whether name is a selector with no model part:
// auto, a native adapter, or a first-class ACP alias.
func knownBareAgentName(name AgentName) bool {
	if name == AgentAuto || nativeAgentNames[name] {
		return true
	}
	_, ok := ACPAliasFor(name)
	return ok
}

// AgentModelUnsupportedReason reports whether a harness rejects a per-run model
// and why. Every ACP selector rejects one; so do "auto" and any native adapter
// with no verified model knob.
func AgentModelUnsupportedReason(harness AgentName) (string, bool) {
	if _, ok := ACPTargetFor(harness); ok {
		return acpModelUnsupportedReason, true
	}
	reason, ok := modelUnsupportedReason[harness]
	return reason, ok
}

// AgentSupportsModel reports whether a harness accepts a per-run model.
func AgentSupportsModel(harness AgentName) bool {
	_, unsupported := AgentModelUnsupportedReason(harness)
	return !unsupported
}

// SplitOpenCodeModel splits an opencode model id into the providerID/modelID
// pair opencode's HTTP API requires. opencode model ids are <provider>/<model>
// and a provider id never contains a slash, so the split is on the FIRST slash
// and the model id keeps any remaining ones.
func SplitOpenCodeModel(model string) (providerID, modelID string, ok bool) {
	provider, id, found := strings.Cut(model, "/")
	if !found || provider == "" || id == "" {
		return "", "", false
	}
	return provider, id, true
}

// ValidAgentSelector reports whether name is usable as the pipeline agent
// selector. It centralizes the selector-name check shared by config parsing and
// the per-run `axi run --agent` override so both accept exactly the set the
// agent factory can construct. It does not probe whether the underlying binary
// is installed - only that the name is a shape the daemon knows how to build.
func ValidAgentSelector(name AgentName) bool {
	_, err := ParseAgentSelector(string(name))
	return err == nil
}
