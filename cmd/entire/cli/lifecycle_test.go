package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// mockLifecycleAgent is a minimal Agent implementation for lifecycle tests.
type mockLifecycleAgent struct {
	name           agent.AgentName
	agentType      agent.AgentType
	transcriptData []byte
	transcriptErr  error
}

var _ agent.Agent = (*mockLifecycleAgent)(nil)

func (m *mockLifecycleAgent) Name() agent.AgentName                  { return m.name }
func (m *mockLifecycleAgent) Type() agent.AgentType                  { return m.agentType }
func (m *mockLifecycleAgent) Description() string                    { return "Mock agent for lifecycle tests" }
func (m *mockLifecycleAgent) IsPreview() bool                        { return false }
func (m *mockLifecycleAgent) DetectPresence() (bool, error)          { return false, nil }
func (m *mockLifecycleAgent) ProtectedDirs() []string                { return nil }
func (m *mockLifecycleAgent) HookNames() []string                    { return nil }
func (m *mockLifecycleAgent) GetSessionID(_ *agent.HookInput) string { return "" }

//nolint:nilnil // Mock implementation
func (m *mockLifecycleAgent) ParseHookEvent(_ string, _ io.Reader) (*agent.Event, error) {
	return nil, nil
}

func (m *mockLifecycleAgent) ReadTranscript(_ string) ([]byte, error) {
	if m.transcriptErr != nil {
		return nil, m.transcriptErr
	}
	return m.transcriptData, nil
}

func (m *mockLifecycleAgent) ChunkTranscript(content []byte, _ int) ([][]byte, error) {
	return [][]byte{content}, nil
}

func (m *mockLifecycleAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	var result []byte
	for _, c := range chunks {
		result = append(result, c...)
	}
	return result, nil
}

func (m *mockLifecycleAgent) GetSessionDir(_ string) (string, error) {
	return "", nil
}

func (m *mockLifecycleAgent) ResolveSessionFile(sessionDir, agentSessionID string) string {
	return filepath.Join(sessionDir, agentSessionID+".jsonl")
}

//nolint:nilnil // Mock implementation
func (m *mockLifecycleAgent) ReadSession(_ *agent.HookInput) (*agent.AgentSession, error) {
	return nil, nil
}

func (m *mockLifecycleAgent) WriteSession(_ *agent.AgentSession) error {
	return nil
}

func (m *mockLifecycleAgent) FormatResumeCommand(_ string) string {
	return ""
}

func newMockAgent() *mockLifecycleAgent {
	return &mockLifecycleAgent{
		name:           "mock-lifecycle",
		agentType:      "Mock Lifecycle Agent",
		transcriptData: []byte(`{"type":"user","message":"test"}`),
	}
}

// --- DispatchLifecycleEvent tests ---

func TestDispatchLifecycleEvent_NilAgent(t *testing.T) {
	t.Parallel()

	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: "test-session",
	}

	err := DispatchLifecycleEvent(nil, event)
	if err == nil {
		t.Error("expected error for nil agent, got nil")
	}
	if !strings.Contains(err.Error(), "agent cannot be nil") {
		t.Errorf("expected error message about nil agent, got: %v", err)
	}
}

func TestDispatchLifecycleEvent_NilEvent(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()

	err := DispatchLifecycleEvent(ag, nil)
	if err == nil {
		t.Error("expected error for nil event, got nil")
	}
	if !strings.Contains(err.Error(), "event cannot be nil") {
		t.Errorf("expected error message about nil event, got: %v", err)
	}
}

func TestDispatchLifecycleEvent_UnknownEventType(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.EventType(999), // Unknown type
		SessionID: "test-session",
	}

	err := DispatchLifecycleEvent(ag, event)
	if err == nil {
		t.Error("expected error for unknown event type, got nil")
	}
	if !strings.Contains(err.Error(), "unknown lifecycle event type") {
		t.Errorf("expected error message about unknown event type, got: %v", err)
	}
}

// --- handleLifecycleSessionStart tests ---

func TestHandleLifecycleSessionStart_EmptySessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "", // Empty
	}

	err := handleLifecycleSessionStart(ag, event)
	if err == nil {
		t.Error("expected error for empty session ID, got nil")
	}
	if !strings.Contains(err.Error(), "no session_id") {
		t.Errorf("expected error message about missing session_id, got: %v", err)
	}
}

// --- handleLifecycleTurnStart tests ---

func TestHandleLifecycleTurnStart_EmptySessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: "", // Empty
	}

	err := handleLifecycleTurnStart(ag, event)
	if err == nil {
		t.Error("expected error for empty session ID, got nil")
	}
	if !strings.Contains(err.Error(), "no session_id") {
		t.Errorf("expected error message about missing session_id, got: %v", err)
	}
}

// --- handleLifecycleTurnEnd tests ---

func TestHandleLifecycleTurnEnd_EmptyTranscriptRef(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "test-session",
		SessionRef: "", // Empty transcript path
	}

	err := handleLifecycleTurnEnd(ag, event)
	if err == nil {
		t.Error("expected error for empty transcript ref, got nil")
	}
	if !strings.Contains(err.Error(), "transcript file not specified") {
		t.Errorf("expected error about transcript file, got: %v", err)
	}
}

func TestHandleLifecycleTurnEnd_NonexistentTranscript(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "test-session",
		SessionRef: "/nonexistent/path/to/transcript.jsonl",
	}

	err := handleLifecycleTurnEnd(ag, event)
	if err == nil {
		t.Error("expected error for nonexistent transcript, got nil")
	}
	if !strings.Contains(err.Error(), "transcript file not found") {
		t.Errorf("expected error about transcript file, got: %v", err)
	}
}

func TestHandleLifecycleTurnEnd_EmptyRepository(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize an empty git repo (no commits)
	if err := os.MkdirAll(".git/objects", 0o755); err != nil {
		t.Fatalf("Failed to create .git: %v", err)
	}
	if err := os.WriteFile(".git/HEAD", []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}
	paths.ClearRepoRootCache()

	// Create a transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o644); err != nil {
		t.Fatalf("Failed to create transcript: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "test-session",
		SessionRef: transcriptPath,
	}

	err := handleLifecycleTurnEnd(ag, event)

	// Should return a SilentError wrapping ErrEmptyRepository
	if err == nil {
		t.Error("expected error for empty repository, got nil")
	}

	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Errorf("expected SilentError, got: %T", err)
	}
	if !errors.Is(silentErr.Unwrap(), strategy.ErrEmptyRepository) {
		t.Errorf("expected ErrEmptyRepository, got: %v", silentErr.Unwrap())
	}
}

// --- handleLifecycleCompaction tests ---

func TestHandleLifecycleCompaction_ResetsTranscriptOffset(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with a commit (not empty)
	setupGitRepoWithCommit(t, tmpDir)
	paths.ClearRepoRootCache()

	// Create .entire directory structure
	if err := os.MkdirAll(paths.EntireDir, 0o755); err != nil {
		t.Fatalf("Failed to create .entire: %v", err)
	}

	// Create a transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	transcriptContent := `{"type":"user","message":{"role":"user","content":"test prompt"}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(transcriptContent), 0o644); err != nil {
		t.Fatalf("Failed to create transcript: %v", err)
	}

	sessionID := "compaction-test-session"

	// Create session state with non-zero transcript offset
	sessionState := &strategy.SessionState{
		SessionID:                 sessionID,
		CheckpointTranscriptStart: 50, // Non-zero offset to be reset
	}
	if err := strategy.SaveSessionState(sessionState); err != nil {
		t.Fatalf("Failed to save session state: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.Compaction,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
	}

	// handleLifecycleCompaction resets the transcript offset regardless of other operations.
	// It fires EventCompaction which stays in ACTIVE phase and resets CheckpointTranscriptStart.
	err := handleLifecycleCompaction(ag, event)
	if err != nil {
		t.Logf("handleLifecycleCompaction returned error (expected in minimal test): %v", err)
	}

	// Verify CheckpointTranscriptStart was reset to 0
	loadedState, loadErr := strategy.LoadSessionState(sessionID)
	if loadErr != nil {
		t.Fatalf("Failed to load session state after compaction: %v", loadErr)
	}
	if loadedState == nil {
		t.Fatal("Session state is nil after compaction")
	}
	if loadedState.CheckpointTranscriptStart != 0 {
		t.Errorf("CheckpointTranscriptStart = %d, want 0 (should be reset on compaction)",
			loadedState.CheckpointTranscriptStart)
	}
}

// --- handleLifecycleSessionEnd tests ---

func TestHandleLifecycleSessionEnd_EmptySessionID(t *testing.T) {
	t.Parallel()

	ag := newMockAgent()
	event := &agent.Event{
		Type:      agent.SessionEnd,
		SessionID: "", // Empty
	}

	// Empty session ID should return nil (no error, just no-op)
	err := handleLifecycleSessionEnd(ag, event)
	if err != nil {
		t.Errorf("expected no error for empty session ID on SessionEnd, got: %v", err)
	}
}

// --- resolveTranscriptOffset tests ---

func TestResolveTranscriptOffset_PrefersPrePromptState(t *testing.T) {
	t.Parallel()

	preState := &PrePromptState{
		TranscriptOffset: 42,
	}

	offset := resolveTranscriptOffset(preState, "test-session")
	if offset != 42 {
		t.Errorf("expected offset 42 from pre-prompt state, got %d", offset)
	}
}

func TestResolveTranscriptOffset_NilPrePromptState(t *testing.T) {
	t.Parallel()

	// With nil pre-prompt state and no session state, should return 0
	offset := resolveTranscriptOffset(nil, "nonexistent-session")
	if offset != 0 {
		t.Errorf("expected offset 0 for nil pre-prompt state, got %d", offset)
	}
}

func TestResolveTranscriptOffset_ZeroOffsetInPrePromptState(t *testing.T) {
	t.Parallel()

	preState := &PrePromptState{
		TranscriptOffset: 0, // Zero should fall through to session state
	}

	// With zero in pre-prompt state and no session state, should return 0
	offset := resolveTranscriptOffset(preState, "nonexistent-session")
	if offset != 0 {
		t.Errorf("expected offset 0, got %d", offset)
	}
}

// --- createContextFile tests ---

func TestCreateContextFile_Format(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	contextFile := filepath.Join(tmpDir, "context.md")

	prompts := []string{"What is the meaning of life?", "Follow-up question here"}
	summary := "This session explored philosophical questions."

	err := createContextFile(contextFile, "feat: add philosophy", "session-123", prompts, summary)
	if err != nil {
		t.Fatalf("createContextFile failed: %v", err)
	}

	content, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("failed to read context file: %v", err)
	}

	contentStr := string(content)

	// Check for expected sections
	if !strings.Contains(contentStr, "# Session Context") {
		t.Error("expected '# Session Context' header")
	}
	if !strings.Contains(contentStr, "Session ID: session-123") {
		t.Error("expected session ID in context file")
	}
	if !strings.Contains(contentStr, "Commit Message: feat: add philosophy") {
		t.Error("expected commit message in context file")
	}
	if !strings.Contains(contentStr, "## Prompts") {
		t.Error("expected '## Prompts' section")
	}
	if !strings.Contains(contentStr, "### Prompt 1") {
		t.Error("expected '### Prompt 1' subsection")
	}
	if !strings.Contains(contentStr, "What is the meaning of life?") {
		t.Error("expected first prompt content")
	}
	if !strings.Contains(contentStr, "### Prompt 2") {
		t.Error("expected '### Prompt 2' subsection")
	}
	if !strings.Contains(contentStr, "## Summary") {
		t.Error("expected '## Summary' section")
	}
	if !strings.Contains(contentStr, "philosophical questions") {
		t.Error("expected summary content")
	}
}

func TestCreateContextFile_EmptyPrompts(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	contextFile := filepath.Join(tmpDir, "context.md")

	err := createContextFile(contextFile, "fix: bug", "session-456", nil, "")
	if err != nil {
		t.Fatalf("createContextFile failed: %v", err)
	}

	content, err := os.ReadFile(contextFile)
	if err != nil {
		t.Fatalf("failed to read context file: %v", err)
	}

	contentStr := string(content)

	// Should still have header and session info
	if !strings.Contains(contentStr, "# Session Context") {
		t.Error("expected '# Session Context' header")
	}
	// Should NOT have prompts section when empty
	if strings.Contains(contentStr, "## Prompts") {
		t.Error("unexpected '## Prompts' section when prompts are empty")
	}
	// Should NOT have summary section when empty
	if strings.Contains(contentStr, "## Summary") {
		t.Error("unexpected '## Summary' section when summary is empty")
	}
}

// --- Event type routing tests ---

func TestDispatchLifecycleEvent_RoutesToCorrectHandler(t *testing.T) {
	t.Parallel()

	// Test that each event type is routed (we can't easily verify which handler
	// was called without dependency injection, but we can verify no panic and
	// expected error types for each event type with minimal required data)

	testCases := []struct {
		name        string
		eventType   agent.EventType
		sessionID   string
		expectError bool
		errorSubstr string
	}{
		{
			name:        "SessionStart with empty session ID",
			eventType:   agent.SessionStart,
			sessionID:   "",
			expectError: true,
			errorSubstr: "no session_id",
		},
		{
			name:        "TurnStart with empty session ID",
			eventType:   agent.TurnStart,
			sessionID:   "",
			expectError: true,
			errorSubstr: "no session_id",
		},
		{
			name:        "TurnEnd with empty transcript",
			eventType:   agent.TurnEnd,
			sessionID:   "test",
			expectError: true,
			errorSubstr: "transcript file not specified",
		},
		{
			name:        "Compaction with empty transcript is no-op",
			eventType:   agent.Compaction,
			sessionID:   "test",
			expectError: false, // Compaction just resets offset; doesn't read transcript
		},
		{
			name:        "SessionEnd with empty session ID is no-op",
			eventType:   agent.SessionEnd,
			sessionID:   "",
			expectError: false,
		},
		{
			name:        "SubagentStart with valid data",
			eventType:   agent.SubagentStart,
			sessionID:   "test",
			expectError: true, // Will fail due to CapturePreTaskState needing git repo
			errorSubstr: "failed to capture pre-task state",
		},
		{
			name:        "SubagentEnd with valid data",
			eventType:   agent.SubagentEnd,
			sessionID:   "test",
			expectError: false, // Succeeds when run from a valid git repo
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ag := newMockAgent()
			event := &agent.Event{
				Type:      tc.eventType,
				SessionID: tc.sessionID,
				Timestamp: time.Now(),
			}

			err := DispatchLifecycleEvent(ag, event)

			if tc.expectError {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tc.errorSubstr)
				} else if !strings.Contains(err.Error(), tc.errorSubstr) {
					t.Errorf("expected error containing %q, got: %v", tc.errorSubstr, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			}
		})
	}
}

// --- Helper functions for test setup ---

// setupGitRepoWithCommit initializes a git repo with an initial commit.
func setupGitRepoWithCommit(t *testing.T, dir string) {
	t.Helper()

	// Initialize git repo
	if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("Failed to create .git/objects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatalf("Failed to create .git/refs/heads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("Failed to create HEAD: %v", err)
	}

	// Create a dummy file to commit
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("Failed to create README.md: %v", err)
	}

	// Use go-git to create an initial commit
	repo, err := strategy.OpenRepository()
	if err != nil {
		// If we can't open with go-git, the empty repo check will work differently
		t.Logf("Note: Could not open repository with go-git: %v", err)
		return
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Logf("Note: Could not get worktree: %v", err)
		return
	}

	if _, err := wt.Add("README.md"); err != nil {
		t.Logf("Note: Could not add file: %v", err)
		return
	}

	if _, err := wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	}); err != nil {
		t.Logf("Note: Could not create commit: %v", err)
	}
}
