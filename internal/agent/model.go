package agent

import "strings"

// Per-harness model flags. Each one is the flag that harness's real CLI takes:
//
//	claude   --model <model>
//	codex    -m <model>            (codex also accepts --model)
//	pi       --model <pattern>     (accepts "provider/id")
//	copilot  --model <model>
//
// opencode is not here: it is driven over HTTP, and the model rides in the
// message body as {providerID, modelID} (see opencode_http.go sendMessage).
// rovodev and every ACP target have no model channel at all and are refused by
// types.ParseAgentSelector before an adapter is ever built.
var (
	claudeModelFlags  = []string{"--model"}
	codexModelFlags   = []string{"-m", "--model"}
	piModelFlags      = []string{"--model"}
	copilotModelFlags = []string{"--model"}
	// opencodeServeModelFlags is stripped from `opencode serve` extras only so
	// a server-level default cannot compete with the explicit per-message
	// model; the message body is what actually selects the model.
	opencodeServeModelFlags = []string{"--model"}
)

// dropModelArgs removes model-selecting flags, and their values, from
// user-supplied agent_args_override extras.
//
// PRECEDENCE: an explicit per-run `--agent <harness>:<model>` must win over
// agent_args_override. Appending our flag after the user's would leave the
// outcome to each CLI's duplicate-flag rule (last-wins, first-wins, or an
// error), which differs per tool and is not something to bet a run on.
// Removing the competing flag makes the precedence deterministic and identical
// across harnesses. It is a no-op when no per-run model was selected, so
// configs that set --model through agent_args_override are unaffected.
func dropModelArgs(args []string, flags []string) []string {
	if len(args) == 0 || len(flags) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		matched := false
		for _, flag := range flags {
			if arg == flag {
				// Consume the flag and its value.
				i++
				matched = true
				break
			}
			if strings.HasPrefix(arg, flag+"=") {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		out = append(out, arg)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
