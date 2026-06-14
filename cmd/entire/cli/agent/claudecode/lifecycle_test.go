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

func TestWaitForTranscriptFlush_RecentFile_WaitsForSentinel(t *testing.T) {
	t.Parallel()

	// Create a transcript file with recent mtime (no sentinel present)
	transcriptFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(transcriptFile, []byte(`{"type":"human"}`+"\n"), 0o644); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}
	// File was just created, so mtime is now — should NOT skip the wait

	start := time.Now()
	waitForTranscriptFlush(context.Background(), transcriptFile, time.Now())
	elapsed := time.Since(start)

	// Should wait close to maxWait (3s) since no sentinel will be found
	if elapsed < 2*time.Second {
		t.Errorf("expected to wait ~3s for recent file without sentinel, but only took %v", elapsed)
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

const testClaudeProjectsBase = "/home/u/.claude/projects"

func TestWorktreeParentCandidate(t *testing.T) {
	t.Parallel()

	reported := filepath.Join(testClaudeProjectsBase, "-Users-foo-Development-repo--claude-worktrees-feature", "sess-1.jsonl")
	got := worktreeParentCandidate(testClaudeProjectsBase, reported, "sess-1")
	want := filepath.Join(testClaudeProjectsBase, "-Users-foo-Development-repo", "sess-1.jsonl")
	if got != want {
		t.Errorf("worktreeParentCandidate = %q, want %q", got, want)
	}
}

func TestWorktreeParentCandidate_NoMarker(t *testing.T) {
	t.Parallel()

	reported := filepath.Join(testClaudeProjectsBase, "-Users-foo-Development-repo", "sess-1.jsonl")
	if got := worktreeParentCandidate(testClaudeProjectsBase, reported, "sess-1"); got != "" {
		t.Errorf("expected empty (no marker), got %q", got)
	}
}

func TestWorktreeParentCandidate_OutsideBase(t *testing.T) {
	t.Parallel()

	if got := worktreeParentCandidate("/a/base", "/somewhere/else/sess-1.jsonl", "sess-1"); got != "" {
		t.Errorf("expected empty (outside base), got %q", got)
	}
}

// TestWorktreeParentCandidate_MarkerInRepoRoot covers a repo whose sanitized
// root already contains the literal "--claude-worktrees-" token (e.g. checked
// out under a directory literally named "acme--claude-worktrees-tools"). Only
// the trailing, synthetic occurrence — the suffix Claude appends from
// .claude/worktrees/<branch> — should be stripped. Cutting at the first
// occurrence would point at the wrong project dir and re-introduce the
// dropped-checkpoint bug for that class of repos.
func TestWorktreeParentCandidate_MarkerInRepoRoot(t *testing.T) {
	t.Parallel()

	parent := "-Users-me-acme--claude-worktrees-tools-repo"
	worktree := parent + "--claude-worktrees-feature"
	reported := filepath.Join(testClaudeProjectsBase, worktree, "sess-1.jsonl")
	got := worktreeParentCandidate(testClaudeProjectsBase, reported, "sess-1")
	want := filepath.Join(testClaudeProjectsBase, parent, "sess-1.jsonl")
	if got != want {
		t.Errorf("worktreeParentCandidate = %q, want %q (must strip only the trailing synthetic marker)", got, want)
	}
}

func TestWorktreeParentCandidate_MarkerAtStart(t *testing.T) {
	t.Parallel()

	// Marker at index 0 of the project segment is meaningless — there is no
	// parent path to recover. Helper returns "".
	reported := filepath.Join(testClaudeProjectsBase, "--claude-worktrees-feature", "sess-1.jsonl")
	if got := worktreeParentCandidate(testClaudeProjectsBase, reported, "sess-1"); got != "" {
		t.Errorf("expected empty (marker at start), got %q", got)
	}
}

func TestResolveTranscriptPath_PassthroughWhenExists(t *testing.T) {
	t.Parallel()

	transcript := filepath.Join(t.TempDir(), "real.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ag := &ClaudeCodeAgent{}
	got := ag.resolveTranscriptPath(transcript, "any-session-id")
	if got != transcript {
		t.Errorf("resolveTranscriptPath returned %q, want passthrough %q", got, transcript)
	}
}

func TestResolveTranscriptPath_EmptyInputs(t *testing.T) {
	t.Parallel()

	ag := &ClaudeCodeAgent{}
	if got := ag.resolveTranscriptPath("", "id"); got != "" {
		t.Errorf("empty sessionRef should pass through; got %q", got)
	}
	if got := ag.resolveTranscriptPath("/some/path", ""); got != "/some/path" {
		t.Errorf("empty sessionID should pass through; got %q", got)
	}
}

// TestResolveTranscriptPath_WorktreeFallback simulates the Claude Code worktree
// bug: the agent reports a transcript_path under a "--claude-worktrees-<branch>"
// project dir that doesn't exist, while the actual transcript was written under
// the parent repo's project dir. The resolver should find the real file by
// scanning the projects base dir for the session ID.
func TestResolveTranscriptPath_WorktreeFallback(t *testing.T) {
	// Cannot t.Parallel() — uses t.Setenv on HOME (process-global).
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	base := filepath.Join(tmpHome, ".claude", "projects")
	parentDir := filepath.Join(base, "-Users-foo-Development-repo")
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		t.Fatalf("mkdir parent project: %v", err)
	}
	sessionID := "wt-session-uuid"
	realPath := filepath.Join(parentDir, sessionID+".jsonl")
	if err := os.WriteFile(realPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// Reported path encodes the worktree CWD — does not exist on disk.
	reported := filepath.Join(base, "-Users-foo-Development-repo--claude-worktrees-feature", sessionID+".jsonl")

	ag := &ClaudeCodeAgent{}
	got := ag.resolveTranscriptPath(reported, sessionID)
	if got != realPath {
		t.Errorf("resolveTranscriptPath = %q, want %q (real location)", got, realPath)
	}
}

// TestResolveTranscriptPath_NoMarker_Passthrough verifies that when the
// reported path is under the projects base but the project segment does not
// carry the worktree marker, the resolver returns the original path unchanged
// rather than fabricating a candidate.
func TestResolveTranscriptPath_NoMarker_Passthrough(t *testing.T) {
	// Cannot t.Parallel() — uses t.Setenv on HOME.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	base := filepath.Join(tmpHome, ".claude", "projects")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	reported := filepath.Join(base, "-Users-foo-Development-repo", "missing.jsonl")

	ag := &ClaudeCodeAgent{}
	got := ag.resolveTranscriptPath(reported, "any-id")
	if got != reported {
		t.Errorf("resolveTranscriptPath = %q, want passthrough %q", got, reported)
	}
}

// TestResolveTranscriptPath_OutsideBaseDir verifies the resolver does not scan
// when the reported path is outside the Claude projects base dir — scanning
// elsewhere couldn't produce a more correct answer and just adds I/O.
func TestResolveTranscriptPath_OutsideBaseDir(t *testing.T) {
	// Cannot t.Parallel() — uses t.Setenv on HOME.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// A real transcript exists under the projects base, but the reported path
	// is somewhere else entirely. Resolver should not redirect.
	base := filepath.Join(tmpHome, ".claude", "projects")
	dir := filepath.Join(base, "some-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sessionID := "outside-session"
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	reported := filepath.Join(t.TempDir(), "elsewhere", sessionID+".jsonl") // not under base
	ag := &ClaudeCodeAgent{}
	got := ag.resolveTranscriptPath(reported, sessionID)
	if got != reported {
		t.Errorf("resolveTranscriptPath = %q, want passthrough %q (path outside base)", got, reported)
	}
}

// TestResolveTranscriptPath_RejectsTraversalSessionID verifies that a session
// ID containing path separators is rejected before being used in filepath.Join.
func TestResolveTranscriptPath_RejectsTraversalSessionID(t *testing.T) {
	// Cannot t.Parallel() — uses t.Setenv on HOME.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	base := filepath.Join(tmpHome, ".claude", "projects", "proj")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A file with a traversal-shaped name exists; the resolver must NOT match it.
	traversalID := "../../etc/passwd"
	reported := filepath.Join(tmpHome, ".claude", "projects", "proj", "missing.jsonl")

	ag := &ClaudeCodeAgent{}
	got := ag.resolveTranscriptPath(reported, traversalID)
	if got != reported {
		t.Errorf("resolveTranscriptPath = %q, want passthrough %q (traversal id rejected)", got, reported)
	}
}
