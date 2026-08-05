package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestOpencodeAgent_CloseWithoutServer(t *testing.T) {
	a := &opencodeAgent{bin: "opencode"}
	if err := a.Close(); err != nil {
		t.Errorf("Close without server should not error: %v", err)
	}
}

// TestOpencodeAgent_FullFlow tests the full session lifecycle using a mock HTTP server.
func TestOpencodeAgent_FullFlow(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.Method+" "+r.URL.Path] = true
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"test-session-456"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Error("expected Accept: text/event-stream")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Send text delta events then usage and idle
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"test-session-456\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"{\\\"success\\\":true,\\\"summary\\\":\\\"all good\\\"}\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"test-session-456\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"tokens\":{\"input\":100,\"output\":50}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/test-session-456/message" && r.Method == http.MethodPost:
			// Return message response with structured output
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{"success":true,"summary":"all good"},"tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"{\"success\":true,\"summary\":\"all good\"}"}]}`)

		case r.URL.Path == "/session/test-session-456" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}

	// Verify structured output from response
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["success"] != true {
		t.Errorf("expected success=true, got %v", output["success"])
	}

	// Verify usage
	if result.Usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", result.Usage.OutputTokens)
	}

	// Verify chunks received
	if len(chunks) < 1 {
		t.Error("expected at least 1 chunk")
	}

	// Verify key API calls were made
	if !calledPaths["POST /session"] {
		t.Error("expected POST /session call")
	}
	if !calledPaths["GET /global/event"] {
		t.Error("expected GET /global/event call")
	}
	if !calledPaths["POST /session/test-session-456/message"] {
		t.Error("expected POST /session/{id}/message call")
	}
}

func TestOpencodeAgent_BackfillsAssistantTextWhenStreamCannotClassifyOrphans(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.Method+" "+r.URL.Path] = true
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"test-session-789"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"test-session-789\",\"field\":\"text\",\"partID\":\"p1\",\"delta\":\"hello \"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"test-session-789\",\"field\":\"text\",\"partID\":\"p2\",\"delta\":\"world\"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"test-session-789\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"tokens\":{\"input\":100,\"output\":50}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/test-session-789/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{"summary":"hello world"},"tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/test-session-789" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("expected one backfilled chunk, got %v", chunks)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected result text 'hello world', got %q", result.Text)
	}
	if string(result.Output) != `{"summary":"hello world"}` {
		t.Fatalf("expected structured summary 'hello world', got %s", string(result.Output))
	}
	if !calledPaths[http.MethodGet+" /global/event"] {
		t.Fatal("expected event stream to be called")
	}
}

func TestOpencodeAgent_BackfillsAllAssistantResponseParts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected combined response text, got %q", result.Text)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("expected one combined backfill chunk, got %v", chunks)
	}
}

func TestOpencodeAgent_BackfillsMissingResponseSuffixAfterStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected streamed and backfilled text to form full response, got %q from %v", got, chunks)
	}
	if len(chunks) != 2 || chunks[1] != " world" {
		t.Fatalf("expected missing suffix backfill, got %v", chunks)
	}
}

func TestOpencodeAgent_BackfillsMissingResponseSuffixAfterToolStep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"step1\",\"messageID\":\"msg1\",\"type\":\"step-finish\",\"tokens\":{\"input\":10,\"output\":5}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello\n\n world" {
		t.Fatalf("expected streamed and backfilled text with separator, got %q from %v", got, chunks)
	}
	if len(chunks) != 3 || chunks[1] != "\n\n" || chunks[2] != " world" {
		t.Fatalf("expected separator before missing suffix backfill, got %v", chunks)
	}
}

func TestOpencodeAgent_DoesNotSeparateBackfillWhenToolStepPrecedesFirstText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"step1\",\"messageID\":\"msg1\",\"type\":\"step-finish\",\"tokens\":{\"input\":10,\"output\":5}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected streamed and backfilled text without separator, got %q from %v", got, chunks)
	}
	if len(chunks) != 2 || chunks[1] != " world" {
		t.Fatalf("expected suffix backfill without separator, got %v", chunks)
	}
}

// TestOpencodeAgent_NoSchema tests the flow without a JSON schema.
func TestOpencodeAgent_NoSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"s1\",\"field\":\"text\",\"partID\":\"p1\",\"delta\":\"done\"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"done"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "hello",
		CWD:    t.TempDir(),
		// No JSONSchema
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if result.Text != "done" {
		t.Fatalf("expected plain text result, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
}

// TestOpencodeAgent_FinalAnswerPreferred tests that final_answer phase text is preferred.
func TestOpencodeAgent_FinalAnswerPreferred(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			// First text part (regular), then final_answer part
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"type\":\"text\",\"text\":\"thinking...\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p2\",\"type\":\"text\",\"text\":\"{\\\"answer\\\":42}\",\"metadata\":{\"openai\":{\"phase\":\"final_answer\"}}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"thinking..."},{"type":"text","text":"{\"answer\":42}","metadata":{"openai":{"phase":"final_answer"}}}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "what is 6*7",
		CWD:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != `{"answer":42}` {
		t.Fatalf("expected final_answer text, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
}

// TestOpencodeAgent_StructuredOutputError asserts that when opencode returns
// an info.error with name=StructuredOutputError, the agent surfaces a
// precise error and does NOT fall through to text-parsing the streamed
// reasoning prose. Regression: run 01KWDTFPNXTC94YEYCN23XFFG1 surfaced
// "invalid character 'N' looking for beginning of value" instead of the
// real cause.
func TestOpencodeAgent_StructuredOutputError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Stream reasoning prose (no JSON) - this is exactly the
			// shape real opencode emits when the model never calls the
			// StructuredOutput tool.
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"Now I need to find the failing test. The only failing test is foo.\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			// opencode signals structured-output failure via
			// info.error.name = "StructuredOutputError". The body
			// intentionally omits info.structured.
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"StructuredOutputError","message":"Model did not produce structured output","retries":2}},"parts":[{"type":"text","text":"Now I need to find the failing test. The only failing test is foo."}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "fix the failing tests",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %+v", result)
	}
	msg := err.Error()
	if !strings.Contains(msg, "structured output failed") {
		t.Errorf("expected error to mention structured output failure, got %q", msg)
	}
	if !strings.Contains(msg, "2 internal retries") {
		t.Errorf("expected error to surface retry count, got %q", msg)
	}
	if strings.Contains(msg, "invalid character") {
		t.Errorf("error must not be the JSON-parse-on-prose symptom, got %q", msg)
	}
	if strings.Contains(msg, "Now I need to find the failing test") {
		t.Errorf("error must not embed the reasoning prose snippet, got %q", msg)
	}
}

// opencodeProseRepairServer is a mock opencode whose first schema-bearing turn
// answers in prose and whose second turn answers with whatever the caller
// chose. It records every message body so a test can assert what the repair
// turn actually asked for.
type opencodeProseRepairServer struct {
	mu            sync.Mutex
	messageBodies []string
	streamTexts   []string
	messageBodyFn func(turn int) string
	// messageStatusFn optionally fails a given turn's message POST with an HTTP
	// status, so a test can make the repair turn fail in transport rather than
	// in formatting. nil means every turn succeeds.
	messageStatusFn func(turn int) int
	// sessionCreates counts POST /session. The mock answers every one with the
	// same id, so this is the only way a test can tell "the repair turn reused
	// the session" from "the repair turn opened a second one and lost the
	// context the model had already built".
	sessionCreates int
}

func (s *opencodeProseRepairServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	streams := 0
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			s.mu.Lock()
			s.sessionCreates++
			s.mu.Unlock()
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			s.mu.Lock()
			turn := streams
			streams++
			text := ""
			if turn < len(s.streamTexts) {
				text = s.streamTexts[turn]
			}
			s.mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			part, err := json.Marshal(text)
			if err != nil {
				t.Errorf("marshal stream text: %v", err)
				return
			}
			fmt.Fprintf(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p%d\",\"messageID\":\"msg%d\",\"type\":\"text\",\"text\":%s}}}}\n\n", turn, turn, part)
			fmt.Fprintf(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg%d\",\"role\":\"assistant\"}}}}\n\n", turn)
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read message body: %v", err)
				return
			}
			s.mu.Lock()
			turn := len(s.messageBodies)
			s.messageBodies = append(s.messageBodies, string(body))
			s.mu.Unlock()
			if s.messageStatusFn != nil {
				if status := s.messageStatusFn(turn); status != 0 && status != http.StatusOK {
					w.WriteHeader(status)
					fmt.Fprint(w, "upstream rejected the follow-up")
					return
				}
			}
			fmt.Fprint(w, s.messageBodyFn(turn))

		default:
			w.WriteHeader(http.StatusOK)
		}
	}
}

func (s *opencodeProseRepairServer) sentPrompts(t *testing.T) []string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	prompts := make([]string, 0, len(s.messageBodies))
	for _, raw := range s.messageBodies {
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("unmarshal sent message %q: %v", raw, err)
		}
		text := ""
		for _, part := range body.Parts {
			text += part.Text
		}
		prompts = append(prompts, text)
	}
	return prompts
}

// TestOpencodeAgent_ProseReplyIsRepairedInSession asserts that a schema-bearing
// turn answered conversationally is re-asked once, in the SAME session, for the
// JSON alone - instead of failing the whole step on a formatting miss after the
// model already did the work. Regression: `--agent
// opencode:github-copilot/gpt-4.1` failed the review step 2/2 runs with
// "invalid character 'N' looking for beginning of value" because gpt-4.1 replied
// in prose and opencode reported neither structured output nor an error.
func TestOpencodeAgent_ProseReplyIsRepairedInSession(t *testing.T) {
	mock := &opencodeProseRepairServer{
		streamTexts: []string{
			"Nothing is in progress or blocked - your repo's localStorage bug is fully fixed.",
			`{"findings":[],"risk_level":"low"}`,
		},
		messageBodyFn: func(turn int) string {
			if turn == 0 {
				return `{"info":{"id":"msg0","role":"assistant"},"parts":[{"type":"text","text":"Nothing is in progress or blocked - your repo's localStorage bug is fully fixed."}]}`
			}
			return `{"info":{"id":"msg1","role":"assistant","structured":{"findings":[],"risk_level":"low"}},"parts":[{"type":"text","text":"{\"findings\":[],\"risk_level\":\"low\"}"}]}`
		},
	}
	server := httptest.NewServer(mock.handler(t))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"risk_level":{"type":"string"}},"required":["risk_level"]}`),
	})
	if err != nil {
		t.Fatalf("expected the repair turn to recover the step, got error: %v", err)
	}
	if result.Output == nil {
		t.Fatalf("expected structured output from the repair turn, got %+v", result)
	}
	var got struct {
		RiskLevel string `json:"risk_level"`
	}
	if err := json.Unmarshal(result.Output, &got); err != nil {
		t.Fatalf("unmarshal structured output %s: %v", result.Output, err)
	}
	if got.RiskLevel != "low" {
		t.Errorf("expected risk_level from the repair turn, got %q", got.RiskLevel)
	}

	prompts := mock.sentPrompts(t)
	if len(prompts) != 2 {
		t.Fatalf("expected exactly one repair turn (2 messages), got %d", len(prompts))
	}
	// The repair turn is only worth doing because the model keeps the context it
	// already built; a second session would throw that away and ask a cold model
	// for the JSON of work it never did.
	mock.mu.Lock()
	sessions := mock.sessionCreates
	mock.mu.Unlock()
	if sessions != 1 {
		t.Errorf("expected the repair turn to reuse the one session, got %d session creates", sessions)
	}
	repair := prompts[1]
	for _, want := range []string{"was prose, not JSON", "do not call any tools", "must match this schema exactly"} {
		if !strings.Contains(repair, want) {
			t.Errorf("repair prompt missing %q, got %q", want, repair)
		}
	}
}

// TestOpencodeAgent_ProseReplyAfterFailedRepairNamesTheCause asserts the
// operator-facing error when even the JSON-only follow-up comes back as prose:
// it must say the model answered in prose, not just surface the raw
// json.Unmarshal symptom, which reads like a daemon defect.
func TestOpencodeAgent_ProseReplyAfterFailedRepairNamesTheCause(t *testing.T) {
	mock := &opencodeProseRepairServer{
		streamTexts: []string{
			"Run npm install in the web/ directory first.",
			// The prose deliberately contains a transient needle: the model's own
			// words must not be able to classify the failure as a network blip and
			// buy itself another full paid attempt.
			"I already told you: run npm install --prefix web, the connection refused nothing.",
		},
		messageBodyFn: func(turn int) string {
			return fmt.Sprintf(`{"info":{"id":"msg%d","role":"assistant"},"parts":[]}`, turn)
		},
	}
	server := httptest.NewServer(mock.handler(t))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"risk_level":{"type":"string"}},"required":["risk_level"]}`),
	})
	if err == nil {
		t.Fatalf("expected an error, got result %+v", result)
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %+v", result)
	}
	msg := err.Error()
	if !strings.Contains(msg, "answered in prose") {
		t.Errorf("expected the error to name prose as the cause, got %q", msg)
	}
	if !strings.Contains(msg, "Run npm install") && !strings.Contains(msg, "I already told you") {
		t.Errorf("expected the error to keep an output snippet for diagnosis, got %q", msg)
	}
	if got := len(mock.sentPrompts(t)); got != 2 {
		t.Errorf("expected exactly one repair turn (2 messages), got %d", got)
	}
	if label, retry := classifyTransient(err); retry {
		t.Errorf("prose quoting a transient needle must not be retried, got label %q", label)
	}
}

// TestOpencodeAgent_FailedRepairTransportIsNotReportedAsProse asserts that when
// the repair turn cannot be delivered at all, the step reports that transport
// failure rather than blaming the model's formatting. Two reasons: an operator
// told "the model answered in prose" would go swap models while the real
// problem is the connection, and classifyTransient matches on the error string,
// so a transport failure must keep its own wording to stay retriable. The
// model's prose is deliberately kept out of that string - it is arbitrary
// output and must not be able to inject a transient needle.
func TestOpencodeAgent_FailedRepairTransportIsNotReportedAsProse(t *testing.T) {
	const prose = "Everything looks fine, no connection refused anywhere."
	mock := &opencodeProseRepairServer{
		streamTexts: []string{prose, ""},
		messageBodyFn: func(turn int) string {
			return fmt.Sprintf(`{"info":{"id":"msg%d","role":"assistant"},"parts":[]}`, turn)
		},
		messageStatusFn: func(turn int) int {
			if turn == 1 {
				return http.StatusBadRequest
			}
			return http.StatusOK
		},
	}
	server := httptest.NewServer(mock.handler(t))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"risk_level":{"type":"string"}},"required":["risk_level"]}`),
	})
	if err == nil {
		t.Fatalf("expected an error, got result %+v", result)
	}
	msg := err.Error()
	if !strings.Contains(msg, "opencode message:") {
		t.Errorf("expected the transport failure to keep its own wording so classifyTransient can see it, got %q", msg)
	}
	if !strings.Contains(msg, "follow-up could not be sent") {
		t.Errorf("expected the error to say the follow-up could not be sent, got %q", msg)
	}
	if strings.Contains(msg, prose) {
		t.Errorf("model prose must not reach the classified error string, got %q", msg)
	}
	if label, retry := classifyTransient(err); retry {
		t.Errorf("a 400 on the repair turn must not be classified transient, got label %q", label)
	}
}

// TestOpencodeAgent_NonStructuredAssistantErrorIsSurfaced asserts that every
// assistant-error variant is named, not only StructuredOutputError. Swallowing
// one and text-parsing the leftover prose reports a provider failure as
// "invalid character ... looking for beginning of value".
func TestOpencodeAgent_NonStructuredAssistantErrorIsSurfaced(t *testing.T) {
	mock := &opencodeProseRepairServer{
		streamTexts: []string{"Let me start by reading the failing test."},
		messageBodyFn: func(int) string {
			return `{"info":{"id":"msg0","role":"assistant","error":{"name":"ContextOverflowError","message":"input exceeds the context window"}},"parts":[]}`
		},
	}
	server := httptest.NewServer(mock.handler(t))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"risk_level":{"type":"string"}},"required":["risk_level"]}`),
	})
	if err == nil {
		t.Fatalf("expected an error, got result %+v", result)
	}
	msg := err.Error()
	if !strings.Contains(msg, "ContextOverflowError") {
		t.Errorf("expected the provider error name, got %q", msg)
	}
	if !strings.Contains(msg, "input exceeds the context window") {
		t.Errorf("expected the provider error message, got %q", msg)
	}
	if strings.Contains(msg, "invalid character") {
		t.Errorf("error must not be the JSON-parse-on-prose symptom, got %q", msg)
	}
	if got := len(mock.sentPrompts(t)); got != 1 {
		t.Errorf("an assistant error must not earn a repair turn, got %d messages", got)
	}
}

// TestOpencodeAgent_NullStructuredOutputIsNotTrusted asserts a literal
// "structured": null is treated as no structured output. It unmarshals into any
// step's findings struct without error, so trusting it would silently record an
// empty, fabricated result instead of failing or repairing.
func TestOpencodeAgent_NullStructuredOutputIsNotTrusted(t *testing.T) {
	mock := &opencodeProseRepairServer{
		streamTexts: []string{"All good.", `{"risk_level":"low"}`},
		messageBodyFn: func(turn int) string {
			if turn == 0 {
				return `{"info":{"id":"msg0","role":"assistant","structured":null},"parts":[{"type":"text","text":"All good."}]}`
			}
			return `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"{\"risk_level\":\"low\"}"}]}`
		},
	}
	server := httptest.NewServer(mock.handler(t))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"risk_level":{"type":"string"}},"required":["risk_level"]}`),
	})
	if err != nil {
		t.Fatalf("expected the repair turn to recover the step, got error: %v", err)
	}
	if string(result.Output) == "null" {
		t.Fatalf("a null structured output must never be trusted as a result")
	}
	if !strings.Contains(string(result.Output), `"low"`) {
		t.Errorf("expected the repaired JSON reply, got %s", result.Output)
	}
}
