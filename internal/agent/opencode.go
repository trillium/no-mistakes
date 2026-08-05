package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// opencodeAgent starts a persistent HTTP server via `opencode serve`
// and sends requests via REST with SSE streaming.
type opencodeAgent struct {
	bin       string
	extraArgs []string
	// providerID/modelID are the split form of the per-run model from an
	// `--agent opencode:<provider>/<model>` selector, both empty when none was
	// chosen. opencode is driven over HTTP, so the model is not a CLI flag: it
	// rides in each message body (see sendMessage). With both empty, the body
	// carries no model field and opencode uses its own configured default,
	// exactly as before.
	providerID string
	modelID    string
	mu         sync.Mutex
	server     *managedServer
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) ReportsAgentAttempts() bool { return true }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "opencode", opts, claudeMaxRetries, classifyTransient, a.recoverTransientRetry, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *opencodeAgent) recoverTransientRetry(label string) {
	if label != "connection refused" {
		return
	}
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.mu.Unlock()
	if srv != nil {
		srv.shutdown()
	}
}

func (a *opencodeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	// Start server on first invocation (synchronized)
	baseURL, err := a.ensureServer(ctx, opts.CWD)
	if err != nil {
		return nil, err
	}

	// Create session with blanket permissions
	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	// Build prompt with schema instructions if provided
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}

	state := &opencodeStreamState{
		sessionID:  sessionID,
		onChunk:    opts.OnChunk,
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}

	resp, err := a.sendTurn(ctx, baseURL, sessionID, prompt, opts, state)
	if err != nil {
		return nil, err
	}

	result, parseErr := opencodeTurnResult(resp, opts.JSONSchema, state)
	if parseErr == nil || len(opts.JSONSchema) == 0 {
		return result, parseErr
	}
	// A weaker model routinely answers a schema-bearing step conversationally:
	// opencode reports no structured output and no error, and the reply is
	// prose. Re-ask once inside the SAME session for the JSON alone before
	// failing the step, so a whole run is not lost to a formatting miss after
	// the model has already done the work. Only a text-parse failure earns the
	// repair round; an assistant error (including StructuredOutputError, which
	// opencode already retried internally) is returned as-is by
	// opencodeTurnResult and never re-asked.
	var notJSON *opencodeTextNotJSONError
	if !errors.As(parseErr, &notJSON) {
		return nil, parseErr
	}
	repairResp, repairErr := a.sendTurn(ctx, baseURL, sessionID, buildOpencodeRepairPrompt(opts.JSONSchema), opts, state)
	if repairErr != nil {
		// The repair turn failed to complete at all, which is a transport
		// failure and not the model's formatting. Return that error so
		// classifyTransient still sees its own wording (a dead server here is
		// worth a retry) and so an operator is not told the model answered in
		// prose when the real problem was the connection. The prose reply's own
		// text is deliberately left out: it is arbitrary model output and must
		// not be able to put a transient needle into the classified string.
		return nil, fmt.Errorf("opencode answered in prose and the JSON-only follow-up could not be sent: %w", repairErr)
	}
	repaired, repairParseErr := opencodeTurnResult(repairResp, opts.JSONSchema, state)
	if repairParseErr == nil {
		return repaired, nil
	}
	var repairNotJSON *opencodeTextNotJSONError
	if errors.As(repairParseErr, &repairNotJSON) {
		return nil, opencodeProseError(repairParseErr)
	}
	return nil, repairParseErr
}

// sendTurn runs one prompt/response exchange against an existing session and
// returns opencode's message response, with state carrying the streamed text
// and accumulated usage. It is a method-scoped extraction of what runOnce used
// to do inline, so a schema-repair round can reuse the same session.
func (a *opencodeAgent) sendTurn(
	ctx context.Context,
	baseURL, sessionID, prompt string,
	opts RunOpts,
	state *opencodeStreamState,
) (*opencodeMessageResponse, error) {
	state.beginTurn()

	// Connect to SSE event stream
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	eventBody, err := a.connectEventStream(streamCtx, baseURL)
	if err != nil {
		return nil, err
	}
	defer eventBody.Close()

	// Send message concurrently — blocks until agent completes
	type messageResult struct {
		resp *opencodeMessageResponse
		err  error
	}
	msgCtx, msgCancel := context.WithCancel(ctx)
	defer msgCancel()
	msgCh := make(chan messageResult, 1)
	go func() {
		resp, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, opts.JSONSchema)
		msgCh <- messageResult{resp: resp, err: err}
	}()

	// Process SSE events until session.idle
	err = parseOpencodeSSE(eventBody, state)
	streamCancel()

	if err != nil {
		// Check if message request failed
		select {
		case mr := <-msgCh:
			if mr.err != nil {
				return nil, fmt.Errorf("opencode message: %w", mr.err)
			}
		default:
		}
		a.abortSession(baseURL, sessionID)
		return nil, fmt.Errorf("opencode events: %w", err)
	}

	// Wait for message response
	mr := <-msgCh
	if mr.err != nil {
		return nil, fmt.Errorf("opencode message: %w", mr.err)
	}

	// Update usage and text from message response
	responseText := ""
	responseFinalText := ""
	if mr.resp != nil && mr.resp.Info != nil {
		streamedText := state.lastText
		streamedFinalText := state.lastFinalText
		emitResponseChunk := func(chunk string) {
			if opts.OnChunk == nil || chunk == "" {
				return
			}
			state.emitSeparatorIfNeeded()
			opts.OnChunk(chunk)
			state.hasEmittedText = true
		}
		if mr.resp.Info.Role == "assistant" && mr.resp.Info.Tokens != nil {
			state.usageByMsg[mr.resp.Info.ID] = opencodeTokensToUsage(mr.resp.Info.Tokens)
			state.usage = accumulateUsage(state.usageByMsg)
		}
		for _, part := range mr.resp.Parts {
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			responseText += part.Text
			if part.Metadata != nil && part.Metadata.OpenAI != nil && part.Metadata.OpenAI.Phase == "final_answer" {
				responseFinalText += part.Text
			}
		}
		if responseText != "" {
			state.lastText = responseText
		}
		if responseFinalText != "" {
			state.lastFinalText = responseFinalText
		}
		if responseFinalText != "" {
			responseText = responseFinalText
		}
		if opts.OnChunk != nil && responseText != "" {
			streamedResponseText := streamedText
			if streamedFinalText != "" {
				streamedResponseText = streamedFinalText
			}
			switch {
			case !state.hasEmittedText:
				emitResponseChunk(responseText)
			case streamedResponseText == "":
				emitResponseChunk(responseText)
			case strings.HasPrefix(responseText, streamedResponseText):
				suffix := responseText[len(streamedResponseText):]
				emitResponseChunk(suffix)
			}
		}
	}

	return mr.resp, nil
}

// opencodeTextNotJSONError marks the one failure a schema-repair round can fix:
// opencode reported no structured output and no error, and the reply text is
// not the JSON the caller's schema requires. It is a marker only - Error()
// delegates so the operator still reads the underlying parse failure and its
// output snippet, with no wrapper sentence of its own.
type opencodeTextNotJSONError struct{ err error }

func (e *opencodeTextNotJSONError) Error() string { return e.err.Error() }

func (e *opencodeTextNotJSONError) Unwrap() error { return e.err }

// opencodeTurnResult converts one exchange into a Result, preferring the
// structured output opencode enforced, then surfacing any assistant error, and
// only then parsing JSON out of the reply text.
func opencodeTurnResult(resp *opencodeMessageResponse, schema json.RawMessage, state *opencodeStreamState) (*Result, error) {
	if structured := opencodeStructuredOutput(resp); structured != nil {
		return &Result{
			Output:                structured,
			Text:                  state.lastText,
			Usage:                 state.usage,
			UsageReported:         state.usage.Reported,
			CacheCreationReported: state.usage.CacheCreationReported,
		}, nil
	}

	if err := opencodeAssistantError(resp); err != nil {
		return nil, err
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	result, err := finalizeTextResult("opencode", outputText, schema, state.usage)
	if err != nil && len(schema) > 0 {
		return nil, &opencodeTextNotJSONError{err: err}
	}
	return result, err
}

// opencodeStructuredOutput returns the schema-conforming output opencode
// enforced, or nil when it reported none. A literal `null` counts as none: it
// unmarshals into any step's findings struct without error, so trusting it
// would record an empty, entirely fabricated result instead of failing.
func opencodeStructuredOutput(resp *opencodeMessageResponse) json.RawMessage {
	if resp == nil || resp.Info == nil || resp.Info.Structured == nil {
		return nil
	}
	switch strings.TrimSpace(string(resp.Info.Structured)) {
	case "", "null":
		return nil
	}
	return resp.Info.Structured
}

// opencodeAssistantError surfaces opencode's own assistant error instead of
// letting the adapter text-parse whatever prose the failed turn left behind.
// StructuredOutputError (the model never called the StructuredOutput tool,
// after opencode's own internal retries) gets the retry count spelled out;
// every other variant - provider auth, output length, context overflow, API,
// content filter, aborted, unknown - is named rather than swallowed, because
// reporting any of them as "invalid character 'N' looking for beginning of
// value" reads like a daemon bug instead of the provider failure it is.
func opencodeAssistantError(resp *opencodeMessageResponse) error {
	if resp == nil || resp.Info == nil || resp.Info.Error == nil || resp.Info.Error.Name == "" {
		return nil
	}
	respErr := resp.Info.Error
	if respErr.IsStructuredOutput() {
		retries := 0
		if respErr.Retries != nil {
			retries = *respErr.Retries
		}
		return fmt.Errorf("opencode structured output failed after %d internal retries: %s",
			retries, respErr.Message)
	}
	if message := strings.TrimSpace(respErr.Message); message != "" {
		return fmt.Errorf("opencode assistant error %s: %s", respErr.Name, message)
	}
	return fmt.Errorf("opencode assistant error %s", respErr.Name)
}

// opencodeProseError names the real cause for an operator. The bare
// json.Unmarshal text ("invalid character 'N' looking for beginning of value")
// describes the symptom of a step two minutes in and reads like a daemon
// defect rather than "the model answered in prose".
// It quotes the model's reply, so it is marked neverTransient: without that, a
// reply containing "connection refused" would be classified as a network blip,
// and the retry would tear down and restart the opencode server to pay for a
// second full run of a step the model simply answered in the wrong shape.
func opencodeProseError(err error) error {
	return neverTransient(fmt.Errorf(
		"opencode answered in prose instead of the JSON this step requires, and a JSON-only follow-up did not recover it "+
			"(the model may be too weak for this step; try a stronger --agent opencode:<provider>/<model>): %w",
		err,
	))
}

func (a *opencodeAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.shutdown()
		a.server = nil
	}
	return nil
}
