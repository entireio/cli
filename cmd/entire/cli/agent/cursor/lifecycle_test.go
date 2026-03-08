package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "test-session-123", "transcript_path": "/tmp/transcript.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
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

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "sess-456", "transcript_path": "/tmp/t.jsonl", "prompt": "Hello world"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeSubmitPrompt, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
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

func TestParseHookEvent_TurnStart_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "sess-m", "transcript_path": "/tmp/t.jsonl", "prompt": "hi", "model": "gpt-4o"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeSubmitPrompt, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "gpt-4o" {
		t.Errorf("expected model 'gpt-4o', got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "sess-nm", "transcript_path": "/tmp/t.jsonl", "prompt": "hi"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeSubmitPrompt, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "" {
		t.Errorf("expected empty model, got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart_CLINoTranscriptPath(t *testing.T) {
	// Cannot use t.Parallel() because of t.Setenv
	ag := &CursorAgent{}
	// Set up a temp dir with a flat transcript file
	tmpDir := t.TempDir()
	transcriptFile := filepath.Join(tmpDir, "cli-turn-start.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"role":"user"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", tmpDir)

	// Cursor CLI sends null for transcript_path in BeforeSubmitPrompt
	input := `{"conversation_id": "cli-turn-start", "prompt": "Hello"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeSubmitPrompt, strings.NewReader(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnStart {
		t.Errorf("expected event type %v, got %v", agent.TurnStart, event.Type)
	}
	// SessionRef must be resolved dynamically so InitializeSession sets TranscriptPath
	if event.SessionRef != transcriptFile {
		t.Errorf("expected resolved SessionRef %q, got %q", transcriptFile, event.SessionRef)
	}
	if event.Prompt != "Hello" {
		t.Errorf("expected prompt 'Hello', got %q", event.Prompt)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "sess-789", "transcript_path": "/tmp/stop.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != "sess-789" {
		t.Errorf("expected conversation_id 'sess-789', got %q", event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "ending-session", "transcript_path": "/tmp/end.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionEnd {
		t.Errorf("expected event type %v, got %v", agent.SessionEnd, event.Type)
	}
	if event.SessionID != "ending-session" {
		t.Errorf("expected conversation_id 'ending-session', got %q", event.SessionID)
	}
}

func TestParseHookEvent_TurnEnd_CLINoTranscriptPath(t *testing.T) {
	ag := &CursorAgent{}
	// Set up a temp dir that simulates the Cursor project dir with a flat transcript
	tmpDir := t.TempDir()
	transcriptDir := filepath.Join(tmpDir, "agent-transcripts")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptFile := filepath.Join(transcriptDir, "cli-session-id.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"role":"user"}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", transcriptDir)

	// CLI stop hook: no transcript_path
	input := `{"conversation_id": "cli-session-id", "status": "completed", "loop_count": 3}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.TurnEnd {
		t.Errorf("expected event type %v, got %v", agent.TurnEnd, event.Type)
	}
	if event.SessionID != "cli-session-id" {
		t.Errorf("expected session_id 'cli-session-id', got %q", event.SessionID)
	}
	if event.SessionRef != transcriptFile {
		t.Errorf("expected computed session_ref %q, got %q", transcriptFile, event.SessionRef)
	}
	if event.TurnCount != 3 {
		t.Errorf("expected TurnCount 3, got %d", event.TurnCount)
	}
}

func TestParseHookEvent_SessionEnd_CLINoTranscriptPath(t *testing.T) {
	ag := &CursorAgent{}
	// Set up a temp dir that simulates the Cursor project dir with a flat transcript
	tmpDir := t.TempDir()
	transcriptDir := filepath.Join(tmpDir, "agent-transcripts")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("failed to create transcript dir: %v", err)
	}
	transcriptFile := filepath.Join(transcriptDir, "cli-end-session.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"role":"user"}`), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", transcriptDir)

	// CLI sessionEnd hook: no transcript_path, has richer fields
	input := `{"conversation_id": "cli-end-session", "reason": "user_closed", "duration_ms": 45000, "is_background_agent": false, "final_status": "completed"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SessionEnd {
		t.Errorf("expected event type %v, got %v", agent.SessionEnd, event.Type)
	}
	if event.SessionID != "cli-end-session" {
		t.Errorf("expected session_id 'cli-end-session', got %q", event.SessionID)
	}
	if event.SessionRef != transcriptFile {
		t.Errorf("expected computed session_ref %q, got %q", transcriptFile, event.SessionRef)
	}
	if event.DurationMs != 45000 {
		t.Errorf("expected DurationMs 45000, got %d", event.DurationMs)
	}
}

func TestParseHookEvent_TurnEnd_IDEWithTranscriptPath(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	// IDE stop hook: transcript_path provided — should use it as-is
	input := `{"conversation_id": "ide-session", "transcript_path": "/home/user/.cursor/projects/proj/agent-transcripts/ide-session/ide-session.jsonl", "status": "completed", "loop_count": 5}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.SessionRef != "/home/user/.cursor/projects/proj/agent-transcripts/ide-session/ide-session.jsonl" {
		t.Errorf("expected IDE-provided session_ref, got %q", event.SessionRef)
	}
}

func TestParseHookEvent_SubagentStart(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	inputData := map[string]any{
		"conversation_id": "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"subagent_id":     "sub_abc123",
		"task":            "do something",
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStart, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SubagentStart {
		t.Errorf("expected event type %v, got %v", agent.SubagentStart, event.Type)
	}
	if event.SessionID != "main-session" {
		t.Errorf("expected session_id 'main-session', got %q", event.SessionID)
	}
	if event.ToolUseID != "sub_abc123" {
		t.Errorf("expected tool_use_id 'sub_abc123', got %q", event.ToolUseID)
	}
}

func TestParseHookEvent_SubagentEnd(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	inputData := map[string]any{
		"conversation_id": "main-session",
		"transcript_path": "/tmp/main.jsonl",
		"subagent_id":     "sub_xyz789",
		"task":            "task done",
		"modified_files":  []string{"src/foo.ts", "src/bar.ts"},
	}
	inputBytes, marshalErr := json.Marshal(inputData)
	if marshalErr != nil {
		t.Fatalf("failed to marshal test input: %v", marshalErr)
	}

	event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(string(inputBytes)))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.SubagentEnd {
		t.Errorf("expected event type %v, got %v", agent.SubagentEnd, event.Type)
	}
	if event.ToolUseID != "sub_xyz789" {
		t.Errorf("expected tool_use_id 'sub_xyz789', got %q", event.ToolUseID)
	}
	if len(event.ModifiedFiles) != 2 {
		t.Fatalf("expected 2 modified files, got %d", len(event.ModifiedFiles))
	}
	if event.ModifiedFiles[0] != "src/foo.ts" || event.ModifiedFiles[1] != "src/bar.ts" {
		t.Errorf("expected modified files [src/foo.ts, src/bar.ts], got %v", event.ModifiedFiles)
	}
}

func TestParseHookEvent_PreCompact(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"conversation_id": "compact-session", "transcript_path": "/tmp/compact.jsonl", "context_tokens": 8500, "context_window_size": 16000}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePreCompact, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event == nil {
		t.Fatal("expected event, got nil")
	}
	if event.Type != agent.Compaction {
		t.Errorf("expected event type %v, got %v", agent.Compaction, event.Type)
	}
	if event.SessionID != "compact-session" {
		t.Errorf("expected session_id 'compact-session', got %q", event.SessionID)
	}
	if event.ContextTokens != 8500 {
		t.Errorf("expected ContextTokens 8500, got %d", event.ContextTokens)
	}
	if event.ContextWindowSize != 16000 {
		t.Errorf("expected ContextWindowSize 16000, got %d", event.ContextWindowSize)
	}
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
	input := `{"session_id": "unknown", "transcript_path": "/tmp/unknown.jsonl"}`

	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook-name", strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput_ReturnsError(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}

	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_ConversationIDFallback(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}

	t.Run("uses conversation_id", func(t *testing.T) {
		t.Parallel()
		input := `{"conversation_id": "bingo-id", "transcript_path": "/tmp/t.jsonl"}`

		event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.SessionID != "bingo-id" {
			t.Errorf("expected session_id 'bingo-id' (from conversation_id), got %q", event.SessionID)
		}
	})

	t.Run("conversation_id fallback for turn start", func(t *testing.T) {
		t.Parallel()
		input := `{"conversation_id": "conv-123", "transcript_path": "/tmp/t.jsonl", "prompt": "hi"}`

		event, err := ag.ParseHookEvent(context.Background(), HookNameBeforeSubmitPrompt, strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.SessionID != "conv-123" {
			t.Errorf("expected session_id 'conv-123', got %q", event.SessionID)
		}
	})

	t.Run("conversation_id fallback for subagent start", func(t *testing.T) {
		t.Parallel()
		input := `{"conversation_id": "conv-sub", "transcript_path": "/tmp/t.jsonl", "subagent_id": "s1", "task": "do something"}`

		event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStart, strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.SessionID != "conv-sub" {
			t.Errorf("expected session_id 'conv-sub', got %q", event.SessionID)
		}
	})

	t.Run("conversation_id fallback for subagent end", func(t *testing.T) {
		t.Parallel()
		input := `{"conversation_id": "conv-end", "transcript_path": "/tmp/t.jsonl", "subagent_id": "s2", "task": "do something"}`

		event, err := ag.ParseHookEvent(context.Background(), HookNameSubagentStop, strings.NewReader(input))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.SessionID != "conv-end" {
			t.Errorf("expected session_id 'conv-end', got %q", event.SessionID)
		}
	})
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &CursorAgent{}
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
			hookName:      HookNameBeforeSubmitPrompt,
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
			hookName:      HookNameSubagentStart,
			expectedType:  agent.SubagentStart,
			inputTemplate: `{"conversation_id": "s5", "transcript_path": "/t", "subagent_id": "sub1", "task": "do something"}`,
		},
		{
			hookName:      HookNameSubagentStop,
			expectedType:  agent.SubagentEnd,
			inputTemplate: `{"conversation_id": "s6", "transcript_path": "/t", "subagent_id": "sub2", "task": "do something"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.hookName, func(t *testing.T) {
			t.Parallel()

			ag := &CursorAgent{}
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

			if event == nil {
				t.Fatal("expected event, got nil")
			}
			if event.Type != tc.expectedType {
				t.Errorf("expected event type %v, got %v", tc.expectedType, event.Type)
			}
		})
	}
}

// --- ReadTranscript ---

func TestReadTranscript_Success(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CursorAgent{}
	data, err := ag.ReadTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}
	if len(data) == 0 {
		t.Error("ReadTranscript() returned empty data")
	}

	// Verify it contains the expected Cursor format markers
	content := string(data)
	if !strings.Contains(content, `"role":"user"`) {
		t.Error("transcript missing 'role' field (Cursor uses 'role', not 'type')")
	}
	if !strings.Contains(content, "<user_query>") {
		t.Error("transcript missing <user_query> tags (Cursor wraps user text)")
	}
}

func TestReadTranscript_MissingFile(t *testing.T) {
	t.Parallel()
	ag := &CursorAgent{}
	_, err := ag.ReadTranscript("/nonexistent/path/transcript.jsonl")
	if err == nil {
		t.Fatal("ReadTranscript() should error for missing file")
	}
}

func TestReadTranscript_MatchesReadSession(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	transcriptPath := writeSampleTranscript(t, tmpDir)

	ag := &CursorAgent{}

	// ReadTranscript
	transcriptData, err := ag.ReadTranscript(transcriptPath)
	if err != nil {
		t.Fatalf("ReadTranscript() error = %v", err)
	}

	// ReadSession
	session, err := ag.ReadSession(&agent.HookInput{
		SessionID:  "compare-session",
		SessionRef: transcriptPath,
	})
	if err != nil {
		t.Fatalf("ReadSession() error = %v", err)
	}

	if !bytes.Equal(transcriptData, session.NativeData) {
		t.Error("ReadTranscript() and ReadSession().NativeData should return identical bytes")
	}
}
