package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// claudeMaxRetries is the number of additional attempts past the initial
// invocation. With 3 retries the agent makes up to 4 total attempts before
// surfacing a transient API error to the pipeline.
const claudeMaxRetries = 3

// errNoStructuredOutput is returned when Claude succeeds but omits structured output.
var errNoStructuredOutput = errors.New("claude returned no structured output")

const claudeScannerMaxTokenSize = 256 * 1024 * 1024

// claudeAgent spawns the claude CLI for each invocation.
type claudeAgent struct {
	bin       string
	extraArgs []string
	// model is the per-run model from an `--agent claude:<model>` selector,
	// empty when none was chosen. It is rendered as claude's --model flag.
	model string
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// buildArgs suppresses claude's project-level settings/memory surface.
	disableProjectSettings bool
}

func (a *claudeAgent) Name() string { return "claude" }

// SupportsSessionResume reports claude's native durable-session capability:
// every stream-json event carries a session_id, and `claude -p --resume <id>`
// continues that session in print mode with the same identity.
func (a *claudeAgent) SupportsSessionResume() bool { return true }

func (a *claudeAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions reports whether claude is currently launched with
// the target repo's project-level settings/memory suppressed. It is meaningful
// only under the opt-out (disableProjectSettings): the gate only consults it
// when the repo opted out. It is honest about the EFFECTIVE setting sources -
// claude's project surface (project CLAUDE.md/AGENTS.md, .claude/settings.json,
// and .claude/settings.local.json) is dropped iff the effective
// --setting-sources excludes both `project` and `local`. buildArgs appends
// `--setting-sources user` when the operator did not pin their own; an operator
// override that re-adds `project`/`local` defeats neutralization, so this
// returns false and the gate fails closed. Verified empirically: with project
// memory loaded claude adopts the firstmate identity; with --setting-sources
// user it does not.
func (a *claudeAgent) NeutralizesGateInstructions() bool {
	return a.disableProjectSettings && claudeEffectiveSettingSourcesNeutral(a.extraArgs)
}

func (a *claudeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "claude", opts, claudeMaxRetries, claudeRetryClassifier, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *claudeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	resumeID := ""
	if opts.Session != nil {
		resumeID = opts.Session.ID
	}

	result, usage, pid, err := a.runTurn(ctx, opts.Prompt, opts, resumeID)
	emitAgentStarted(opts, "claude", pid)
	if err != nil {
		emitAgentExited(opts, "claude", pid, err)
		return nil, err
	}

	res, finalErr := finalizeClaudeResult(result, opts.JSONSchema, usage)
	if res != nil {
		res.SessionID = result.sessionID
		res.Resumed = resumeID != ""
		res.Model = result.model
		// Claude reports cache-creation cost per message, so the accumulated
		// value is meaningful (recorded as a real number, not unknown). Its
		// stream-json usage is per-invocation, not cumulative across --resume,
		// so SessionUsageCumulative stays false and per-round deltas equal the
		// raw counters.
		res.CacheCreationReported = res.UsageReported
		if result.model != "" {
			res.ModelProvider = "anthropic"
		}
	}
	if errors.Is(finalErr, errNoStructuredOutput) && opts.OnChunk != nil {
		opts.OnChunk(fmt.Sprintf("structured output missing: subtype=%s, text_len=%d, input_tokens=%d, output_tokens=%d",
			result.Subtype, len(result.text), usage.InputTokens, usage.OutputTokens))
		opts.OnChunk(fmt.Sprintf("raw result event: %s", string(result.rawEvent)))
	}

	// A schema-bearing turn answered in prose (text present but not JSON) earns
	// exactly ONE repair turn via --resume in the same session. The model keeps
	// the context it already built, so we ask only for the JSON rendering of the
	// answer it just gave. Only a text-parse failure earns the repair; a plain
	// errNoStructuredOutput (empty text, transient omission) is left for the
	// runWithRetry loop to handle as before.
	var textNotJSON *claudeTextNotJSONError
	if errors.As(finalErr, &textNotJSON) && result.sessionID != "" && len(opts.JSONSchema) > 0 {
		repairResult, repairUsage, repairPid, repairErr := a.runTurn(ctx, buildClaudeRepairPrompt(opts.JSONSchema), opts, result.sessionID)
		emitAgentStarted(opts, "claude", repairPid)
		if repairErr != nil {
			// The repair turn failed to complete — this is transport, not formatting.
			// Return it unwrapped so classifyTransient can still see the error
			// wording and decide whether a retry is warranted.
			emitAgentExited(opts, "claude", repairPid, repairErr)
			return nil, fmt.Errorf("claude answered in prose and the JSON-only follow-up could not complete: %w", repairErr)
		}
		// Accumulate tokens from both turns so the caller sees the true cost of
		// the repair round, not just the follow-up message in isolation.
		combinedUsage := usage
		combinedUsage.Add(repairUsage)
		repaired, repairFinalErr := finalizeClaudeResult(repairResult, opts.JSONSchema, combinedUsage)
		if repairFinalErr == nil {
			if repaired != nil {
				repaired.SessionID = repairResult.sessionID
				repaired.Model = repairResult.model
				repaired.CacheCreationReported = repaired.UsageReported
				if repairResult.model != "" {
					repaired.ModelProvider = "anthropic"
				}
			}
			emitAgentExited(opts, "claude", repairPid, nil)
			return repaired, nil
		}
		// Repair turn also came back as prose — name the cause for the operator.
		emitAgentExited(opts, "claude", repairPid, repairFinalErr)
		return nil, claudeProseError(finalErr)
	}

	emitAgentExited(opts, "claude", pid, finalErr)
	return res, finalErr
}

// runTurn spawns one claude process, collects its output, and returns the raw
// result. It is extracted from runOnce so the repair round can reuse the same
// session (via resumeID) without going through the full runOnce/runWithRetry
// machinery. The caller is responsible for lifecycle event emission.
func (a *claudeAgent) runTurn(ctx context.Context, prompt string, opts RunOpts, resumeID string) (*claudeResult, TokenUsage, int, error) {
	args := a.buildArgs(opts.JSONSchema, resumeID)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	// Claude Code print mode documents text stdin as its non-interactive
	// prompt transport. Giving os/exec an in-memory reader keeps user prompt
	// bytes out of argv and lets Cmd own the bounded concurrent copy, including
	// EOF, early-child-exit, cancellation, and WaitDelay cleanup paths.
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = gitSafeEnv(opts.CWD)
	shellenv.ConfigureShellCommand(cmd)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	started, err := startNativeAgentCommand(cmd)
	if err != nil {
		return nil, TokenUsage{}, 0, fmt.Errorf("claude start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()

	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	var usage TokenUsage
	var result *claudeResult
	if err := parseClaudeEvents(ctx, started.stdout, opts.OnChunk, &usage, &result); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		return nil, usage, pid, fmt.Errorf("claude parse events: %w", err)
	}

	waitErr := started.wait()
	stderrWG.Wait()
	if waitErr != nil {
		return nil, usage, pid, fmt.Errorf("claude exited: %w: %s", waitErr, string(stderrBuf))
	}

	if result == nil {
		return nil, usage, pid, fmt.Errorf("claude returned no result event")
	}
	return result, usage, pid, nil
}

func (a *claudeAgent) Close() error { return nil }

// claudeTextNotJSONError marks the one case a repair turn can fix: claude
// succeeded but its text output is not the JSON the schema requires, even after
// parseStructuredTextOutput tried to extract it. The session that produced it
// is still live, so --resume can re-ask for the JSON without discarding the
// context the model already built. Error() delegates so the operator reads the
// underlying parse failure without a wrapper sentence.
type claudeTextNotJSONError struct{ err error }

func (e *claudeTextNotJSONError) Error() string { return e.err.Error() }
func (e *claudeTextNotJSONError) Unwrap() error { return e.err }

// buildClaudeRepairPrompt is the single in-session follow-up sent when a
// schema-bearing turn came back as prose. It forbids further work and tool
// calls — the model has already done the task, and the only thing missing is
// the JSON rendering of the answer it just gave.
func buildClaudeRepairPrompt(schema json.RawMessage) string {
	return strings.Join([]string{
		"Your previous reply was prose, not JSON, so it could not be used.",
		"Do not do any more work and do not call any tools.",
		"Reply with only the JSON result for the task you just completed, and nothing else.",
		"The JSON must match this schema exactly: " + string(schema),
	}, "\n")
}

// claudeProseError wraps err in neverTransient so classifyTransient cannot read
// the model's own words as a retriable signal. The operator-readable message
// names prose output as the cause and points at CLAUDE.md output-shaping rules.
func claudeProseError(err error) error {
	return neverTransient(fmt.Errorf(
		"claude answered in prose instead of the JSON this step requires, and a JSON-only follow-up did not recover it "+
			"(output-shaping rules or hooks in CLAUDE.md may prevent structured output in non-interactive runs; "+
			"check whether those rules apply to pipeline agents): %w",
		err,
	))
}

func finalizeClaudeResult(result *claudeResult, schema json.RawMessage, usage TokenUsage) (*Result, error) {
	if result.IsError || result.Subtype != "success" {
		return nil, fmt.Errorf("claude error: subtype=%s", result.Subtype)
	}
	if len(schema) == 0 {
		return &Result{
			Text:                  result.text,
			Usage:                 usage,
			UsageReported:         usage.Reported,
			CacheCreationReported: usage.CacheCreationReported,
		}, nil
	}
	if result.StructuredOutput != nil {
		return &Result{
			Output:                result.StructuredOutput,
			Text:                  result.text,
			Usage:                 usage,
			UsageReported:         usage.Reported,
			CacheCreationReported: usage.CacheCreationReported,
		}, nil
	}
	// Structured-output channel is nil. ADHD output-shaping rules or models that
	// ignore the structured-output contract may embed the required JSON in their
	// text reply instead (e.g. "Status: done.\n\n{...}" or a fenced block).
	// Try text extraction before declaring a failure — this avoids the repair
	// round entirely when the JSON is present but wrapped in prose.
	if result.text != "" {
		output, parseErr := parseStructuredTextOutput(result.text, schema)
		if parseErr == nil {
			return &Result{
				Output:                output,
				Text:                  result.text,
				Usage:                 usage,
				UsageReported:         usage.Reported,
				CacheCreationReported: usage.CacheCreationReported,
			}, nil
		}
		// Text is present but contains no parseable JSON — a repair turn via
		// --resume may recover it by asking for the JSON alone.
		return nil, &claudeTextNotJSONError{
			err: fmt.Errorf("claude output parse: %w (output snippet: %q)", parseErr, outputSnippet(result.text)),
		}
	}
	return nil, errNoStructuredOutput
}

// buildArgs constructs the claude CLI arguments. User-supplied extraArgs
// (from agent_args_override in the global config) are inserted ahead of the
// managed flags, so user choices win over no-mistakes' defaults. If the user
// supplied their own permission mode, the default --dangerously-skip-permissions
// is not added. A non-empty resumeID continues that session via --resume
// (never --fork-session: the session identity must stay stable so later
// turns keep resuming the same conversation).
func (a *claudeAgent) buildArgs(schema json.RawMessage, resumeID string) []string {
	// A per-run selector model wins over agent_args_override: drop the user's
	// competing --model rather than rely on claude's duplicate-flag rule.
	extraArgs := a.extraArgs
	if a.model != "" {
		extraArgs = dropModelArgs(extraArgs, claudeModelFlags)
	}
	args := make([]string, 0, len(extraArgs)+13)
	args = append(args, extraArgs...)
	if a.model != "" {
		args = append(args, "--model", a.model)
	}
	args = append(args,
		"-p",
		"--verbose",
		"--output-format", "stream-json",
	)
	// Project-settings opt-out (trusted-only; see config.DisableProjectSettings):
	// load only user-level settings and memory, never the target repo's
	// project/local CLAUDE.md/AGENTS.md, .claude/settings.json, or
	// .claude/settings.local.json. In an agent-orchestration target (firstmate)
	// the project memory otherwise installs a fleet-captain identity on the gate
	// agent; `--setting-sources user` drops the project and local sources (the
	// full project surface) while preserving the operator's own user-level config
	// and auth. Suppressed only when the operator did not pin their own
	// --setting-sources. When the repo did not opt out, nothing is added and
	// claude loads its project memory exactly as before (backward-compat).
	if a.disableProjectSettings && !claudeUserSetSettingSources(a.extraArgs) {
		args = append(args, "--setting-sources", "user")
	}
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if len(schema) > 0 {
		args = append(args, "--json-schema", string(schema))
	}
	if !claudeUserSetPermissionMode(a.extraArgs) {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

// claudeUserSetSettingSources reports whether extraArgs pin --setting-sources at
// all, in which case buildArgs does not add its own.
func claudeUserSetSettingSources(extraArgs []string) bool {
	_, pinned := claudeUserSettingSources(extraArgs)
	return pinned
}

// claudeUserSettingSources returns the operator-pinned --setting-sources value
// (last occurrence wins) and whether it was pinned. Handles `--setting-sources
// <v>` and `--setting-sources=<v>`.
func claudeUserSettingSources(extraArgs []string) (string, bool) {
	value := ""
	pinned := false
	for i, arg := range extraArgs {
		if arg == "--setting-sources" && i+1 < len(extraArgs) {
			value = extraArgs[i+1]
			pinned = true
		} else if strings.HasPrefix(arg, "--setting-sources=") {
			value = strings.TrimPrefix(arg, "--setting-sources=")
			pinned = true
		}
	}
	return value, pinned
}

// claudeEffectiveSettingSourcesNeutral reports whether the EFFECTIVE claude
// setting sources drop the target repo's project and local surface: true when
// the operator did not pin --setting-sources (buildArgs appends `user`) or
// pinned a value that contains neither `project` nor `local`, and false when the
// operator's value re-adds `project`/`local`.
func claudeEffectiveSettingSourcesNeutral(extraArgs []string) bool {
	value, pinned := claudeUserSettingSources(extraArgs)
	if !pinned {
		return true // buildArgs appends --setting-sources user
	}
	for _, src := range strings.Split(value, ",") {
		switch strings.ToLower(strings.TrimSpace(src)) {
		case "project", "local":
			return false
		}
	}
	return true
}

// claudeUserSetPermissionMode reports whether extraArgs already declare a
// permission flag, in which case buildArgs skips its default.
func claudeUserSetPermissionMode(extraArgs []string) bool {
	for _, arg := range extraArgs {
		if arg == "--dangerously-skip-permissions" ||
			arg == "--permission-mode" ||
			strings.HasPrefix(arg, "--permission-mode=") {
			return true
		}
	}
	return false
}

// claudeEvent is the top-level JSONL event from claude CLI.
type claudeEvent struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message,omitempty"`
	SessionID string          `json:"session_id,omitempty"`

	// result fields
	Subtype          string          `json:"subtype,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Usage            *claudeUsage    `json:"usage,omitempty"`
}

// claudeResult captures the parsed result event.
type claudeResult struct {
	Subtype          string
	IsError          bool
	StructuredOutput json.RawMessage
	text             string // accumulated text from assistant events
	rawEvent         json.RawMessage
	sessionID        string // durable session identity from the event stream
	model            string // model reported by assistant events
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type claudeMessage struct {
	Model   string          `json:"model"`
	Usage   claudeUsage     `json:"usage"`
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseClaudeEvents reads JSONL from the reader and dispatches events.
// It accumulates token usage and captures the final result event.
func parseClaudeEvents(ctx context.Context, r io.Reader, onChunk func(string), usage *TokenUsage, result **claudeResult) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), claudeScannerMaxTokenSize)
	var textBuf string
	var lastSessionID string
	var lastModel string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event claudeEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue // skip malformed lines
		}
		if event.SessionID != "" {
			lastSessionID = event.SessionID
		}

		switch event.Type {
		case "assistant":
			var msg claudeMessage
			if err := json.Unmarshal(event.Message, &msg); err != nil {
				continue
			}
			if msg.Model != "" {
				lastModel = msg.Model
			}
			usage.Add(TokenUsage{
				InputTokens:           msg.Usage.InputTokens,
				OutputTokens:          msg.Usage.OutputTokens,
				CacheReadTokens:       msg.Usage.CacheReadInputTokens,
				CacheCreationTokens:   msg.Usage.CacheCreationInputTokens,
				Reported:              true,
				CacheCreationReported: true,
			})
			for _, c := range msg.Content {
				if c.Type == "text" && c.Text != "" {
					textBuf += c.Text
					if onChunk != nil {
						onChunk(c.Text)
					}
				}
			}

		case "result":
			if result != nil {
				raw := make(json.RawMessage, len(line))
				copy(raw, line)
				*result = &claudeResult{
					Subtype:          event.Subtype,
					IsError:          event.IsError,
					StructuredOutput: event.StructuredOutput,
					text:             textBuf,
					rawEvent:         raw,
					sessionID:        lastSessionID,
					model:            lastModel,
				}
			}
		}
	}

	return scanner.Err()
}
