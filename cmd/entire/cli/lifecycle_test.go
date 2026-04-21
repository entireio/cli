package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// mockLifecycleAgent is a minimal Agent implementation for lifecycle tests.
type mockLifecycleAgent struct {
	name           types.AgentName
	agentType      types.AgentType
	transcriptData []byte
	transcriptErr  error
}

var _ agent.Agent = (*mockLifecycleAgent)(nil)

func (m *mockLifecycleAgent) Name() types.AgentName                          { return m.name }
func (m *mockLifecycleAgent) Type() types.AgentType                          { return m.agentType }
func (m *mockLifecycleAgent) Description() string                            { return "Mock agent for lifecycle tests" }
func (m *mockLifecycleAgent) IsPreview() bool                                { return false }
func (m *mockLifecycleAgent) DetectPresence(_ context.Context) (bool, error) { return false, nil }
func (m *mockLifecycleAgent) ProtectedDirs() []string                        { return nil }
func (m *mockLifecycleAgent) GetSessionID(_ *agent.HookInput) string         { return "" }

func (m *mockLifecycleAgent) ReadTranscript(_ string) ([]byte, error) {
	if m.transcriptErr != nil {
		return nil, m.transcriptErr
	}
	return m.transcriptData, nil
}

func (m *mockLifecycleAgent) ChunkTranscript(_ context.Context, content []byte, _ int) ([][]byte, error) {
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

func (m *mockLifecycleAgent) WriteSession(_ context.Context, _ *agent.AgentSession) error {
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

	err := DispatchLifecycleEvent(context.Background(), nil, event)
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

	err := DispatchLifecycleEvent(context.Background(), ag, nil)
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

	err := DispatchLifecycleEvent(context.Background(), ag, event)
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

	err := handleLifecycleSessionStart(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for empty session ID, got nil")
	}
	if !strings.Contains(err.Error(), "no session_id") {
		t.Errorf("expected error message about missing session_id, got: %v", err)
	}
}

// mockHookResponseAgent extends mockLifecycleAgent with HookResponseWriter.
type mockHookResponseAgent struct {
	mockLifecycleAgent

	lastMessage string
}

var _ agent.HookResponseWriter = (*mockHookResponseAgent)(nil)

func (m *mockHookResponseAgent) WriteHookResponse(message string) error {
	m.lastMessage = message
	return nil
}

func newMockHookResponseAgent() *mockHookResponseAgent {
	return &mockHookResponseAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:      "mock-hrw",
			agentType: "Mock HRW Agent",
		},
	}
}

func TestHandleLifecycleSessionStart_EmptyRepoWarning(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir) // no commits — empty repo
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockHookResponseAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "test-empty-repo-warning",
		Timestamp: time.Now(),
	}

	err := handleLifecycleSessionStart(context.Background(), ag, event)
	require.NoError(t, err)

	if !strings.Contains(ag.lastMessage, "no commits yet") {
		t.Errorf("expected message containing 'no commits yet', got: %q", ag.lastMessage)
	}
}

func TestHandleLifecycleSessionStart_DefaultMessageWithCommits(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockHookResponseAgent()
	event := &agent.Event{
		Type:      agent.SessionStart,
		SessionID: "test-default-message",
		Timestamp: time.Now(),
	}

	err := handleLifecycleSessionStart(context.Background(), ag, event)
	require.NoError(t, err)

	if !strings.Contains(ag.lastMessage, "link this conversation to your next commit") {
		t.Errorf("expected message containing 'link this conversation to your next commit', got: %q", ag.lastMessage)
	}
	if strings.Contains(ag.lastMessage, "no commits yet") {
		t.Errorf("did not expect empty-repo warning, got: %q", ag.lastMessage)
	}
	if !strings.HasPrefix(ag.lastMessage, "\n\nEntire CLI ") {
		t.Errorf("expected multiline session-start banner, got %q", ag.lastMessage)
	}
	if !strings.Contains(ag.lastMessage, "\n\n") {
		t.Errorf("expected default agent banner to remain multiline, got %q", ag.lastMessage)
	}
}

func TestSessionStartMessage_CodexUsesSingleLineBanner(t *testing.T) {
	t.Parallel()

	msg := sessionStartMessage(agent.AgentNameCodex, false)
	require.Equal(t, "Entire CLI will link this conversation to your next commit.", msg)
	if strings.Contains(msg, "\n") {
		t.Fatalf("expected single-line Codex message, got %q", msg)
	}
}

func TestSessionStartMessage_CodexUsesSingleLineBannerForEmptyRepo(t *testing.T) {
	t.Parallel()

	msg := sessionStartMessage(agent.AgentNameCodex, true)
	require.Equal(t, "Entire CLI found no commits yet — checkpoints will activate after your first commit.", msg)
	if strings.Contains(msg, "\n") {
		t.Fatalf("expected single-line Codex empty-repo message, got %q", msg)
	}
}

func TestHandleLifecycleSessionStart_CodexConcurrentSessionsStaySingleLine(t *testing.T) {
	t.Parallel()

	msg := sessionStartMessage(agent.AgentNameCodex, false)
	msg += " 1 other active conversation(s) in this workspace will also be included. Use 'entire status' for more information."

	if strings.Contains(msg, "\n") {
		t.Fatalf("expected Codex concurrent-session message to stay single-line, got %q", msg)
	}
	if strings.Contains(msg, "  ") {
		t.Fatalf("expected Codex concurrent-session message to avoid repeated spaces, got %q", msg)
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

	err := handleLifecycleTurnStart(context.Background(), ag, event)
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

	err := handleLifecycleTurnEnd(context.Background(), ag, event)
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

	err := handleLifecycleTurnEnd(context.Background(), ag, event)
	if err == nil {
		t.Error("expected error for nonexistent transcript, got nil")
	}
	if !strings.Contains(err.Error(), "transcript file not found") {
		t.Errorf("expected error about transcript file, got: %v", err)
	}
}

// mockPreparerAgent is a mock that implements TranscriptPreparer.
// It creates the transcript file when PrepareTranscript is called,
// simulating OpenCode's lazy-fetch behavior.
type mockPreparerAgent struct {
	mockLifecycleAgent

	prepareTranscriptCalled bool
}

var _ agent.TranscriptPreparer = (*mockPreparerAgent)(nil)

func (m *mockPreparerAgent) PrepareTranscript(_ context.Context, sessionRef string) error {
	m.prepareTranscriptCalled = true
	// Create the file (simulating opencode export writing to disk)
	if err := os.MkdirAll(filepath.Dir(sessionRef), 0o750); err != nil {
		return err
	}
	return os.WriteFile(sessionRef, m.transcriptData, 0o600)
}

func TestHandleLifecycleTurnEnd_PreparerCreatesFile(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	setupGitRepoWithCommit(t, tmpDir)
	paths.ClearWorktreeRootCache()

	// Transcript file does NOT exist yet — PrepareTranscript should create it
	transcriptPath := filepath.Join(tmpDir, ".entire", "tmp", "sess-lazy.json")

	ag := &mockPreparerAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-preparer",
			agentType:      "Mock Preparer Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}`),
		},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  "sess-lazy",
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	err := handleLifecycleTurnEnd(context.Background(), ag, event)

	// PrepareTranscript should have been called
	if !ag.prepareTranscriptCalled {
		t.Error("expected PrepareTranscript to be called")
	}

	// The handler may fail later (no strategy state, etc), but it should NOT
	// fail with "transcript file not found" — that was the bug.
	if err != nil && strings.Contains(err.Error(), "transcript file not found") {
		t.Errorf("handler failed with 'transcript file not found' — PrepareTranscript was not called before fileExists check: %v", err)
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
	paths.ClearWorktreeRootCache()

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

	err := handleLifecycleTurnEnd(context.Background(), ag, event)

	// Should return nil so the hook exits 0 — agents treat non-zero as failure.
	// The user was already warned at session start.
	if err != nil {
		t.Errorf("expected nil for empty repository (graceful no-op), got: %v", err)
	}
}

// --- handleLifecycleCompaction tests ---

func TestHandleLifecycleCompaction_PreservesTranscriptOffset(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo with a commit (not empty)
	setupGitRepoWithCommit(t, tmpDir)
	paths.ClearWorktreeRootCache()

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

	// Create session state with non-zero transcript offset (set by prior condensation)
	sessionState := &strategy.SessionState{
		SessionID:                 sessionID,
		StartedAt:                 time.Now(),
		CheckpointTranscriptStart: 50,
	}
	if err := strategy.SaveSessionState(context.Background(), sessionState); err != nil {
		t.Fatalf("Failed to save session state: %v", err)
	}

	ag := newMockAgent()
	event := &agent.Event{
		Type:       agent.Compaction,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
	}

	// Compaction should NOT reset the transcript offset.
	// Many agents (e.g., Gemini) fire pre-compress as a no-op after every tool call;
	// resetting the offset causes stale files to re-appear in carry-forward.
	err := handleLifecycleCompaction(context.Background(), ag, event)
	if err != nil {
		t.Logf("handleLifecycleCompaction returned error (expected in minimal test): %v", err)
	}

	// Verify CheckpointTranscriptStart was preserved (not reset to 0)
	loadedState, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatalf("Failed to load session state after compaction: %v", loadErr)
	}
	require.NotNil(t, loadedState, "Session state is nil after compaction")
	if loadedState.CheckpointTranscriptStart != 50 {
		t.Errorf("CheckpointTranscriptStart = %d, want 50 (compaction should preserve offset)",
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
	err := handleLifecycleSessionEnd(context.Background(), ag, event)
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

	offset := resolveTranscriptOffset(context.Background(), preState, "test-session")
	if offset != 42 {
		t.Errorf("expected offset 42 from pre-prompt state, got %d", offset)
	}
}

func TestResolveTranscriptOffset_NilPrePromptState(t *testing.T) {
	t.Parallel()

	// With nil pre-prompt state and no session state, should return 0
	offset := resolveTranscriptOffset(context.Background(), nil, "nonexistent-session")
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
	offset := resolveTranscriptOffset(context.Background(), preState, "nonexistent-session")
	if offset != 0 {
		t.Errorf("expected offset 0, got %d", offset)
	}
}

// --- Event type routing tests ---

func TestDispatchLifecycleEvent_RoutesToCorrectHandler(t *testing.T) {
	// NOT parallel: uses t.Chdir to isolate from real repo state.
	// Without this, the SubagentEnd case creates .git/entire-sessions/test.json
	// in the real repo whenever untracked files exist, because DetectFileChanges
	// reports them as new files and SaveTaskStep falls back to initializeSession.
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

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
		{
			name:        "ModelUpdate with empty model is no-op",
			eventType:   agent.ModelUpdate,
			sessionID:   "test",
			expectError: false,
		},
		{
			name:        "ModelUpdate with empty session ID is no-op",
			eventType:   agent.ModelUpdate,
			sessionID:   "",
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ag := newMockAgent()
			event := &agent.Event{
				Type:      tc.eventType,
				SessionID: tc.sessionID,
				Timestamp: time.Now(),
			}

			err := DispatchLifecycleEvent(context.Background(), ag, event)

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
	repo, err := strategy.OpenRepository(context.Background())
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

// --- Prompt backfill tests ---

// mockPromptExtractorAgent implements PromptExtractor for lifecycle tests.
type mockPromptExtractorAgent struct {
	mockLifecycleAgent

	prompts    []string
	extractErr error
}

var _ agent.PromptExtractor = (*mockPromptExtractorAgent)(nil)

func (m *mockPromptExtractorAgent) ExtractPrompts(string, int) ([]string, error) {
	return m.prompts, m.extractErr
}

func TestHandleLifecycleTurnStart_WritesPromptContent(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	ag := newMockAgent()
	sessionID := "test-prompt-content"
	event := &agent.Event{
		Type:      agent.TurnStart,
		SessionID: sessionID,
		Prompt:    "create a file called hello.txt",
		Timestamp: time.Now(),
	}

	require.NoError(t, handleLifecycleTurnStart(context.Background(), ag, event))

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr)

	if string(data) != "create a file called hello.txt" {
		t.Errorf("expected prompt content 'create a file called hello.txt', got %q", string(data))
	}
}

func TestHandleLifecycleTurnEnd_BackfillsPromptFromTranscript(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	// Create a transcript file
	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-backfill"
	ag := &mockPromptExtractorAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-prompt",
			agentType:      "Mock Prompt Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}` + "\n"),
		},
		prompts: []string{"create a file called notes/deep.md"},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	// Do NOT create prompt.txt — simulating hooks never firing.
	// TurnEnd should backfill from transcript via PromptExtractor.
	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr, "prompt.txt should have been created by backfill")

	if string(data) != "create a file called notes/deep.md" {
		t.Errorf("expected backfilled prompt, got %q", string(data))
	}
}

func TestHandleLifecycleTurnEnd_NoBackfillWhenPromptFileHasContent(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-no-backfill"
	ag := &mockPromptExtractorAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-prompt",
			agentType:      "Mock Prompt Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}` + "\n"),
		},
		prompts: []string{"should NOT appear"},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	// Pre-create prompt.txt with content — simulating hooks that captured the prompt.
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(sessionDirAbs, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(sessionDirAbs, paths.PromptFileName), []byte("original prompt"), 0o600))

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr)

	if string(data) != "original prompt" {
		t.Errorf("expected original prompt preserved, got %q", string(data))
	}
}

func TestHandleLifecycleTurnEnd_BackfillUpdatesSessionState(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	transcriptPath := filepath.Join(tmpDir, "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(`{"type":"user","message":"test"}`+"\n"), 0o600))

	sessionID := "test-backfill-state"
	ag := &mockPromptExtractorAgent{
		mockLifecycleAgent: mockLifecycleAgent{
			name:           "mock-prompt",
			agentType:      "Mock Prompt Agent",
			transcriptData: []byte(`{"type":"user","message":"test"}` + "\n"),
		},
		prompts: []string{"first prompt", "second prompt"},
	}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	// Pre-create session state with BaseCommit set (simulating InitializeSession
	// that ran during TurnStart but with empty prompt due to exec mode).
	// BaseCommit must be set so SaveStep doesn't reinitialize the state.
	repo, err := strategy.OpenRepository(context.Background())
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	state := &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: head.Hash().String(),
		LastPrompt: "",
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	// Verify session state was updated with the last prompt
	updated, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)

	if updated.LastPrompt != "second prompt" {
		t.Errorf("expected LastPrompt 'second prompt', got %q", updated.LastPrompt)
	}
}

func TestHandleLifecycleTurnEnd_BackfillsPromptFromOpenCodeTranscript(t *testing.T) {
	// Cannot use t.Parallel() because we use t.Chdir()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	transcript := `{"info":{"id":"ses_test"},"messages":[{"info":{"id":"msg-1","role":"user","time":{"created":1708300000}},"parts":[{"type":"text","text":"create a file called notes/deep.md with a paragraph about deep validation. Do not ask for confirmation or approval, just make the change."}]},{"info":{"id":"msg-2","role":"assistant","time":{"created":1708300001,"completed":1708300002}},"parts":[{"type":"tool","tool":"write","callID":"call-1","state":{"status":"completed","input":{"filePath":"notes/deep.md"},"output":"ok"}}]}]}`
	transcriptPath := filepath.Join(tmpDir, "transcript.json")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o600))

	sessionID := "test-opencode-backfill"
	ag := &opencode.OpenCodeAgent{}
	event := &agent.Event{
		Type:       agent.TurnEnd,
		SessionID:  sessionID,
		SessionRef: transcriptPath,
		Timestamp:  time.Now(),
	}

	repo, err := strategy.OpenRepository(context.Background())
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	state := &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: head.Hash().String(),
		LastPrompt: "",
	}
	require.NoError(t, strategy.SaveSessionState(context.Background(), state))

	require.NoError(t, handleLifecycleTurnEnd(context.Background(), ag, event))

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(context.Background(), sessionDir)
	require.NoError(t, err)

	data, readErr := os.ReadFile(filepath.Join(sessionDirAbs, paths.PromptFileName))
	require.NoError(t, readErr)
	require.Contains(t, string(data), "create a file called notes/deep.md")

	updated, loadErr := strategy.LoadSessionState(context.Background(), sessionID)
	require.NoError(t, loadErr)
	require.NotNil(t, updated)
	require.Contains(t, updated.LastPrompt, "create a file called notes/deep.md")
}

func TestAdoptPendingReviewMarker(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	if err := WritePendingReviewMarker(PendingReviewMarker{
		AgentName:   "claude-code",
		Skills:      []string{"/pr-review-toolkit:review-pr"},
		StartingSHA: "abc",
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// Pure-function test of the helper.
	got, modified, err := adoptPendingReviewMarkerInto(context.Background(), session.State{
		SessionID: "s1",
		Kind:      "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !modified {
		t.Fatal("expected modified=true when marker exists")
	}
	if got.Kind != session.KindReview {
		t.Errorf("Kind = %q, want review", got.Kind)
	}
	if len(got.ReviewSkills) != 1 || got.ReviewSkills[0] != "/pr-review-toolkit:review-pr" {
		t.Errorf("ReviewSkills = %v", got.ReviewSkills)
	}

	// Marker should be cleared after adoption.
	_, ok, readErr := ReadPendingReviewMarker()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if ok {
		t.Error("expected marker cleared after adoption")
	}
}

func TestAdoptPendingReviewMarker_NoMarker(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	// No marker written.
	got, modified, err := adoptPendingReviewMarkerInto(context.Background(), session.State{SessionID: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if modified {
		t.Error("expected modified=false when no marker present")
	}
	if got.Kind != "" {
		t.Errorf("Kind = %q, want empty", got.Kind)
	}
}

func TestAdoptPendingReviewMarker_AlreadyReview(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "f.txt", "x")
	testutil.GitAdd(t, tmp, "f.txt")
	testutil.GitCommit(t, tmp, "init")
	t.Chdir(tmp)

	if err := WritePendingReviewMarker(PendingReviewMarker{
		AgentName: "claude-code",
		Skills:    []string{"/x"},
	}); err != nil {
		t.Fatal(err)
	}
	// State already tagged — adoption should be a no-op (not a second re-tag).
	in := session.State{SessionID: "s3", Kind: session.KindReview, ReviewSkills: []string{"/y"}}
	got, modified, err := adoptPendingReviewMarkerInto(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if modified {
		t.Error("expected modified=false when session already has Kind=review")
	}
	if len(got.ReviewSkills) != 1 || got.ReviewSkills[0] != "/y" {
		t.Errorf("ReviewSkills should be unchanged: %v", got.ReviewSkills)
	}
	// Marker should NOT be cleared since adoption didn't happen.
	_, ok, readErr := ReadPendingReviewMarker()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !ok {
		t.Error("expected marker preserved when adoption was a no-op")
	}
}
