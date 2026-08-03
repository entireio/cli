package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "test-session-123", "transcript_path": "/tmp/transcript.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected event type %v, got %v", agent.SessionStart, event.Type)
	}
	if event.SessionID != "test-session-123" {
		t.Errorf("expected session_id 'test-session-123', got %q", event.SessionID)
	}
	if event.SessionRef != "/tmp/transcript.jsonl" {
		t.Errorf("expected session_ref '/tmp/transcript.jsonl', got %q", event.SessionRef)
	}
	if event.Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestParseHookEvent_SessionStart_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "model-session", "transcript_path": "/tmp/t.jsonl", "model": "claude-sonnet-4-20250514"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", event.Model)
	}
}

func TestParseHookEvent_SessionStart_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "no-model-session", "transcript_path": "/tmp/t.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "" {
		t.Errorf("expected empty model, got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "sess-456", "transcript_path": "/tmp/t.jsonl", "prompt": "Hello world"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnStart {
		t.Errorf("expected event type %v, got %v", agent.TurnStart, event.Type)
	}
	if event.SessionID != "sess-456" {
		t.Errorf("expected session_id 'sess-456', got %q", event.SessionID)
	}
	if event.Prompt != "Hello world" {
		t.Errorf("expected prompt 'Hello world', got %q", event.Prompt)
	}
}

// The VS Code extension prepends an <ide_opened_file> context block to the
// prompt; it must be stripped so the session/checkpoint title and prompt show
// only what the user typed.
func TestParseHookEvent_TurnStart_StripsIDEContextTags(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id":"s1","transcript_path":"/tmp/t.jsonl","prompt":"<ide_opened_file>The user opened /a/b.md in the IDE.</ide_opened_file>\n\nrewrite these docs as one plan"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	if event.Prompt != "rewrite these docs as one plan" {
		t.Errorf("IDE context tag not stripped; prompt = %q", event.Prompt)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "sess-789", "transcript_path": "/tmp/stop.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != "sess-789" {
		t.Errorf("expected session_id 'sess-789', got %q", event.SessionID)
	}
}

func TestParseHookEvent_TurnEnd_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "sess-stop-model", "transcript_path": "/tmp/stop.jsonl", "model": "claude-opus-4-6"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "claude-opus-4-6" {
		t.Errorf("expected model 'claude-opus-4-6', got %q", event.Model)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "ending-session", "transcript_path": "/tmp/end.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionEnd {
		t.Errorf("expected event type %v, got %v", agent.SessionEnd, event.Type)
	}
	if event.SessionID != "ending-session" {
		t.Errorf("expected session_id 'ending-session', got %q", event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "end-model", "transcript_path": "/tmp/end.jsonl", "model": "claude-sonnet-4-20250514"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", event.Model)
	}
}

func TestParseHookEvent_SubagentStart(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	toolInput := json.RawMessage(`{"description": "test task", "prompt": "do something"}`)
	inputData := map[string]any{
		"session_id":      "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"tool_use_id":     "toolu_abc123",
		"tool_input":      toolInput,
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreTask, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SubagentStart {
		t.Errorf("expected event type %v, got %v", agent.SubagentStart, event.Type)
	}
	if event.SessionID != "main-session" {
		t.Errorf("expected session_id 'main-session', got %q", event.SessionID)
	}
	if event.ToolUseID != "toolu_abc123" {
		t.Errorf("expected tool_use_id 'toolu_abc123', got %q", event.ToolUseID)
	}
	if event.ToolInput == nil {
		t.Error("expected tool_input to be set")
	}
}

func TestParseHookEvent_SubagentEnd(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	inputData := map[string]any{
		"session_id":      "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"tool_use_id":     "toolu_xyz789",
		"tool_input":      json.RawMessage(`{"prompt": "task done"}`),
		"tool_response": map[string]string{
			"agentId": "agent-subagent-001",
		},
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostTask, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected event type %v, got %v", agent.SubagentEnd, event.Type)
	}
	if event.ToolUseID != "toolu_xyz789" {
		t.Errorf("expected tool_use_id 'toolu_xyz789', got %q", event.ToolUseID)
	}
	if event.SubagentID != "agent-subagent-001" {
		t.Errorf("expected subagent_id 'agent-subagent-001', got %q", event.SubagentID)
	}
}

func TestParseHookEvent_SubagentEnd_NoAgentID(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	inputData := map[string]any{
		"session_id":      "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"tool_use_id":     "toolu_no_agent",
		"tool_input":      json.RawMessage(`{}`),
		"tool_response":   map[string]string{},
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostTask, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.SubagentID != "" {
		t.Errorf("expected empty subagent_id, got %q", event.SubagentID)
	}
}

func TestParseHookEvent_PostTodo_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "todo-session", "transcript_path": "/tmp/todo.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostTodo, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for post-todo, got %+v", event)
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "unknown", "transcript_path": "/tmp/unknown.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook-name", strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}

	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	input := `{"session_id": "test", "transcript_path": INVALID}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse hook input") {
		t.Errorf("expected 'failed to parse hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_AllHookTypes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		hookName      string
		expectedType  agent.EventType
		expectNil     bool
		inputTemplate string
	}{
		{
			hookName:      HookNameSessionStart,
			expectedType:  agent.SessionStart,
			inputTemplate: `{"session_id": "s1", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameUserPromptSubmit,
			expectedType:  agent.TurnStart,
			inputTemplate: `{"session_id": "s2", "transcript_path": "/t", "prompt": "hi"}`,
		},
		{
			hookName:      HookNameStop,
			expectedType:  agent.TurnEnd,
			inputTemplate: `{"session_id": "s3", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNameSessionEnd,
			expectedType:  agent.SessionEnd,
			inputTemplate: `{"session_id": "s4", "transcript_path": "/t"}`,
		},
		{
			hookName:      HookNamePreTask,
			expectedType:  agent.SubagentStart,
			inputTemplate: `{"session_id": "s5", "transcript_path": "/t", "tool_use_id": "t1", "tool_input": {}}`,
		},
		{
			hookName:      HookNamePostTask,
			expectedType:  agent.SubagentEnd,
			inputTemplate: `{"session_id": "s6", "transcript_path": "/t", "tool_use_id": "t2", "tool_input": {}, "tool_response": {}}`,
		},
		{
			hookName:      HookNamePostTodo,
			expectNil:     true,
			inputTemplate: `{"session_id": "s7", "transcript_path": "/t"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.hookName, func(t *testing.T) {
			t.Parallel()

			ag := &ClaudeCodeAgent{}
			event, err := ag.ParseHookEvent(context.Background(), tc.hookName, strings.NewReader(tc.inputTemplate))

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tc.expectNil {
				if event != nil {
					t.Errorf("expected nil event, got %+v", event)
				}
				return
			}

			require.NotNil(t, event, "expected event, got nil")
			if event.Type != tc.expectedType {
				t.Errorf("expected event type %v, got %v", tc.expectedType, event.Type)
			}
		})
	}
}

func TestReadAndParse_ValidInput(t *testing.T) {
	t.Parallel()

	input := `{"session_id": "test-123", "transcript_path": "/path/to/transcript"}`

	result, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, result, "expected result, got nil")
	if result.SessionID != "test-123" {
		t.Errorf("expected session_id 'test-123', got %q", result.SessionID)
	}
	if result.TranscriptPath != "/path/to/transcript" {
		t.Errorf("expected transcript_path '/path/to/transcript', got %q", result.TranscriptPath)
	}
}

func TestReadAndParse_EmptyInput(t *testing.T) {
	t.Parallel()

	_, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestReadAndParse_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader("not valid json"))

	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "failed to parse hook input") {
		t.Errorf("expected 'failed to parse hook input' error, got: %v", err)
	}
}

func TestReadAndParse_PartialJSON(t *testing.T) {
	t.Parallel()

	// JSON with only some fields - should still parse (missing fields are zero values)
	input := `{"session_id": "partial-only"}`

	result, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SessionID != "partial-only" {
		t.Errorf("expected session_id 'partial-only', got %q", result.SessionID)
	}
	if result.TranscriptPath != "" {
		t.Errorf("expected empty transcript_path, got %q", result.TranscriptPath)
	}
}

func TestReadAndParse_ExtraFields(t *testing.T) {
	t.Parallel()

	// JSON with extra fields - should ignore them
	input := `{"session_id": "test", "transcript_path": "/t", "extra_field": "ignored", "another": 123}`

	result, err := agent.ReadAndParseHookInput[sessionInfoRaw](strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SessionID != "test" {
		t.Errorf("expected session_id 'test', got %q", result.SessionID)
	}
}

func TestWaitForTranscriptFlush_StaleFile_SkipsWait(t *testing.T) {
	t.Parallel()

	// Create a transcript file and backdate its mtime to make it "stale"
	transcriptFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
	staleTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(transcriptFile, staleTime, staleTime); err != nil {
		t.Fatalf("failed to set mtime: %v", err)
	}

	// waitForTranscriptFlush should return almost instantly for stale files
	// (not wait the full 3 seconds)
	start := time.Now()
	waitForTranscriptFlush(context.Background(), transcriptFile, time.Now())
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("expected fast return for stale transcript, but took %v", elapsed)
	}
}

func TestWaitForTranscriptFlush_RecentStableFile_ReturnsFast(t *testing.T) {
	t.Parallel()

	// A recent transcript that has stopped growing (the healthy turn-end case:
	// the assistant finished streaming). Even though the "hooks claude-code stop"
	// sentinel is never present in the file, the wait must settle on size
	// stability and return quickly instead of burning the full 3s maxWait.
	transcriptFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	start := time.Now()
	waitForTranscriptFlush(context.Background(), transcriptFile, time.Now())
	elapsed := time.Since(start)

	// A stable file settles once its size has held steady for the quiet window
	// (~500ms) — comfortably under the 3s cap that the old sentinel-only wait
	// always hit.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("expected fast return for a stable recent transcript, but took %v", elapsed)
	}
}

func TestWaitForTranscriptFlush_GrowingFile_WaitsUntilSettled(t *testing.T) {
	t.Parallel()

	// A transcript that is still being written must NOT be treated as settled
	// while it grows; the wait returns only after the writes stop.
	transcriptFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	const growFor = 600 * time.Millisecond
	stop := make(chan struct{})
	go func() {
		f, err := os.OpenFile(transcriptFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(growFor)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if time.Now().After(deadline) {
					return
				}
				if _, werr := f.WriteString(`{"type":"assistant"}` + "\n"); werr != nil {
					return
				}
			}
		}
	}()

	start := time.Now()
	waitForTranscriptFlush(context.Background(), transcriptFile, time.Now())
	elapsed := time.Since(start)
	close(stop)

	// It should keep waiting while the file grows (i.e. not return near-instantly),
	// but still return once writes stop (bounded by the 3s cap).
	if elapsed < 300*time.Millisecond {
		t.Errorf("expected to keep waiting while transcript grew, returned after only %v", elapsed)
	}
	if elapsed > 3500*time.Millisecond {
		t.Errorf("expected to return once settled/within cap, but took %v", elapsed)
	}
}

func TestWaitForTranscriptFlush_BriefMidWritePause_NotDeclaredDoneEarly(t *testing.T) {
	t.Parallel()

	// A writer that stalls briefly mid-write (shorter than the quiet window) and
	// then resumes must NOT be treated as settled during the lull. With a
	// too-short stability check the ~300ms pause below would be mistaken for
	// completion and turn-end would read a truncated transcript.
	transcriptFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	const (
		pauseDur  = 300 * time.Millisecond // stable, but shorter than the 500ms quiet window
		resumeDur = 200 * time.Millisecond // further writes after the pause
	)

	lastWrite := make(chan time.Time, 1)
	go func() {
		f, err := os.OpenFile(transcriptFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			lastWrite <- time.Now()
			return
		}
		defer f.Close()

		// Hold the file size steady for pauseDur — a brief mid-write stall.
		time.Sleep(pauseDur)

		// Resume writing; record when the final write lands.
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(resumeDur)
		last := time.Now()
		for range ticker.C {
			if time.Now().After(deadline) {
				break
			}
			if _, werr := f.WriteString(`{"type":"assistant"}` + "\n"); werr != nil {
				break
			}
			last = time.Now()
		}
		lastWrite <- last
	}()

	start := time.Now()
	waitForTranscriptFlush(context.Background(), transcriptFile, time.Now())
	returnedAt := time.Now()
	elapsed := returnedAt.Sub(start)

	final := <-lastWrite

	// The wait must not have returned during the pause: a truncated early return
	// would land near the old ~100ms stability window, before writing resumed.
	if !returnedAt.After(final) {
		t.Errorf("returned at %v before the writer's final write at %v (declared done during mid-write pause)",
			returnedAt.Sub(start), final.Sub(start))
	}
	// Sanity: it keeps waiting past the pause, and still returns within the cap.
	if elapsed < pauseDur {
		t.Errorf("expected to keep waiting through the mid-write pause, returned after only %v", elapsed)
	}
	if elapsed > 3500*time.Millisecond {
		t.Errorf("expected to return once settled/within cap, but took %v", elapsed)
	}
}

func TestWaitForTranscriptFlush_NonexistentFile_ReturnsImmediately(t *testing.T) {
	t.Parallel()

	// File doesn't exist — os.Stat fails, return immediately (nothing to poll).
	start := time.Now()
	waitForTranscriptFlush(context.Background(), "/nonexistent/transcript.jsonl", time.Now())
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Errorf("expected immediate return for nonexistent file, but took %v", elapsed)
	}
}

func TestClaudeCodeAgent_ContextInjector(t *testing.T) {
	t.Parallel()
	c := &ClaudeCodeAgent{}
	if got := c.InjectionEvent(); got != agent.TurnStart {
		t.Errorf("InjectionEvent = %v, want TurnStart", got)
	}
	out, err := c.RenderContextInjection(agent.ContextInjection{Text: "use entire trail"})
	if err != nil {
		t.Fatalf("RenderContextInjection: %v", err)
	}
	var parsed struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v (%q)", err, string(out))
	}
	if parsed.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit", parsed.HookSpecificOutput.HookEventName)
	}
	if parsed.HookSpecificOutput.AdditionalContext != "use entire trail" {
		t.Errorf("additionalContext = %q", parsed.HookSpecificOutput.AdditionalContext)
	}
}
