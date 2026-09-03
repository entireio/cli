package strategy

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"

	// Register agents so AgentForTranscriptPath can resolve them.
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCursorSessionDir points the Cursor agent at a temp directory and returns
// a sample transcript path inside it.
func withCursorSessionDir(t *testing.T) string {
	t.Helper()
	sessionDir := filepath.Join(t.TempDir(), "agent-transcripts")
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", sessionDir)
	return filepath.Join(sessionDir, "abc-123.jsonl")
}

// withClaudeSessionDir does the same for Claude Code.
func withClaudeSessionDir(t *testing.T) string {
	t.Helper()
	sessionDir := filepath.Join(t.TempDir(), "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", sessionDir)
	return filepath.Join(sessionDir, "abc-123.jsonl")
}

func withCodexSessionDir(t *testing.T) string {
	t.Helper()
	sessionDir := filepath.Join(t.TempDir(), "codex-sessions")
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", sessionDir)
	return filepath.Join(sessionDir, "2026", "09", "02", "rollout.jsonl")
}

func TestInitializeSession_CodexCorrection_CleanKnownOwner(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	codexTranscript := withCodexSessionDir(t)

	ctx := context.Background()
	sessionID := "codex-clean-known"
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "first", ""))
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeCodex, codexTranscript, "second", ""))

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	assertCleanCodexCorrection(t, state)
}

func TestInitializeSession_CodexCorrection_CleanUnknownOwner(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	codexTranscript := withCodexSessionDir(t)

	ctx := context.Background()
	sessionID := "codex-clean-unknown"
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "first", ""))
	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	state.AgentType = ""
	require.NoError(t, s.saveSessionState(ctx, state))

	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeCodex, codexTranscript, "second", ""))
	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	assertCleanCodexCorrection(t, state)
}

func TestInitializeSession_CodexCorrection_DirtyEvidence(t *testing.T) {
	incomplete := false
	tests := []struct {
		name  string
		dirty func(*SessionState)
	}{
		{"inventory", func(s *SessionState) { s.SubagentInventory = []session.SubagentInventoryEntry{{AgentID: "child"}} }},
		{"task record", func(s *SessionState) {
			s.TaskRecords = []session.TaskRecord{{ToolUseID: "task", AgentID: "child-from-task"}}
		}},
		{"ledger", func(s *SessionState) { s.SubagentLedgerVersion = 4 }},
		{"session child total", func(s *SessionState) { s.TokenUsage = &agent.TokenUsage{SubagentTokens: &agent.TokenUsage{}} }},
		{"checkpoint child total", func(s *SessionState) { s.CheckpointTokenUsage = &agent.TokenUsage{SubagentTokens: &agent.TokenUsage{}} }},
		{"baseline", func(s *SessionState) { s.SubagentTokensBaseline = &agent.TokenUsage{} }},
		{"inventory incomplete", func(s *SessionState) { s.SubagentInventoryComplete = &incomplete }},
		{"baseline incomplete", func(s *SessionState) { s.SubagentTokensBaselineComplete = &incomplete }},
		{"session usage incomplete", func(s *SessionState) { s.TokenUsage = &agent.TokenUsage{SubagentTokensComplete: &incomplete} }},
		{"checkpoint usage incomplete", func(s *SessionState) { s.CheckpointTokenUsage = &agent.TokenUsage{SubagentTokensComplete: &incomplete} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupGitRepo(t)
			t.Chdir(dir)
			codexTranscript := withCodexSessionDir(t)
			ctx := context.Background()
			sessionID := "codex-dirty-" + strings.ReplaceAll(tt.name, " ", "-")
			s := &ManualCommitStrategy{}
			require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "first", ""))
			state, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)
			tt.dirty(state)
			require.NoError(t, s.saveSessionState(ctx, state))

			require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeCodex, codexTranscript, "second", ""))
			state, err = s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)
			require.Equal(t, agent.AgentTypeCodex, state.AgentType)
			require.NotNil(t, state.SubagentInventoryComplete)
			require.False(t, *state.SubagentInventoryComplete)
			require.NotNil(t, state.SubagentTokensBaselineComplete)
			require.False(t, *state.SubagentTokensBaselineComplete)
			require.NotNil(t, state.TokenUsage)
			require.Nil(t, state.TokenUsage.SubagentTokens)
			require.NotNil(t, state.TokenUsage.SubagentTokensComplete)
			require.False(t, *state.TokenUsage.SubagentTokensComplete)
			require.NotNil(t, state.CheckpointTokenUsage)
			require.Nil(t, state.CheckpointTokenUsage.SubagentTokens)
			require.NotNil(t, state.CheckpointTokenUsage.SubagentTokensComplete)
			require.False(t, *state.CheckpointTokenUsage.SubagentTokensComplete)
			require.Nil(t, state.SubagentTokensBaseline)
			if tt.name == "task record" {
				require.NotNil(t, state.FindSubagentInventory("child-from-task"))
			}
		})
	}
}

func TestInitializeSession_CodexCallerWithoutTranscriptDoesNotMigrateAccounting(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "codex-unproven-owner"
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "first", ""))
	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	state.AgentType = ""
	state.SubagentTokensBaseline = &agent.TokenUsage{InputTokens: 7}
	require.NoError(t, s.saveSessionState(ctx, state))

	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeCodex, "", "second", ""))
	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, agent.AgentTypeCodex, state.AgentType)
	require.NotNil(t, state.SubagentTokensBaseline)
	require.Equal(t, 7, state.SubagentTokensBaseline.InputTokens,
		"the firing hook alone must not rewrite legacy accounting evidence")
}

func assertCleanCodexCorrection(t *testing.T, state *SessionState) {
	t.Helper()
	require.Equal(t, agent.AgentTypeCodex, state.AgentType)
	require.Empty(t, state.SubagentInventory)
	require.Zero(t, state.SubagentLedgerVersion)
	require.Nil(t, state.SubagentTokensBaseline)
	require.NotNil(t, state.SubagentInventoryComplete)
	require.True(t, *state.SubagentInventoryComplete)
	require.NotNil(t, state.SubagentTokensBaselineComplete)
	require.True(t, *state.SubagentTokensBaselineComplete)
	require.NotNil(t, state.TokenUsage)
	require.Nil(t, state.TokenUsage.SubagentTokens)
	require.NotNil(t, state.TokenUsage.SubagentTokensComplete)
	require.True(t, *state.TokenUsage.SubagentTokensComplete)
	require.NotNil(t, state.CheckpointTokenUsage)
	require.Nil(t, state.CheckpointTokenUsage.SubagentTokens)
	require.NotNil(t, state.CheckpointTokenUsage.SubagentTokensComplete)
	require.True(t, *state.CheckpointTokenUsage.SubagentTokensComplete)
}

func TestResolveSessionAgentType_TranscriptPathBeatsHook(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	cursorTranscript := withCursorSessionDir(t)

	got := resolveSessionAgentType(context.Background(), "test-session-1", agent.AgentTypeClaudeCode, cursorTranscript)
	assert.Equal(t, agent.AgentTypeCursor, got,
		"transcript path inside Cursor's session dir must override the firing hook's agent type")
}

func TestResolveSessionAgentType_HintBeatsHookWhenNoTranscript(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	ctx := context.Background()
	_, err := StoreAgentTypeHint(ctx, "test-session-2", agent.AgentTypeCursor)
	require.NoError(t, err)

	got := resolveSessionAgentType(ctx, "test-session-2", agent.AgentTypeClaudeCode, "")
	assert.Equal(t, agent.AgentTypeCursor, got,
		"SessionStart hint must override the firing hook's agent type when no transcript path is given")
}

func TestResolveSessionAgentType_FallsBackToHook(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	got := resolveSessionAgentType(context.Background(), "test-session-3", agent.AgentTypeClaudeCode, "/somewhere/unrelated.jsonl")
	assert.Equal(t, agent.AgentTypeClaudeCode, got,
		"with no hint and an unrelated transcript path, the hook's agent type wins")
}

func TestResolveSessionAgentType_TranscriptOverridesEvenWithHint(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	cursorTranscript := withCursorSessionDir(t)

	ctx := context.Background()
	// Wrong-first-writer scenario: Claude Code's SessionStart fired first by accident.
	_, err := StoreAgentTypeHint(ctx, "test-session-4", agent.AgentTypeClaudeCode)
	require.NoError(t, err)

	got := resolveSessionAgentType(ctx, "test-session-4", agent.AgentTypeClaudeCode, cursorTranscript)
	assert.Equal(t, agent.AgentTypeCursor, got,
		"transcript path is the strongest signal — even an existing (wrong) hint must lose to it")
}

func TestCorrectSessionAgentType_TranscriptDisagrees(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	cursorTranscript := withCursorSessionDir(t)

	corrected, changed := correctSessionAgentType(context.Background(), agent.AgentTypeClaudeCode, cursorTranscript)
	assert.True(t, changed)
	assert.Equal(t, agent.AgentTypeCursor, corrected)
}

func TestCorrectSessionAgentType_TranscriptAgrees_NoChange(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	cursorTranscript := withCursorSessionDir(t)

	corrected, changed := correctSessionAgentType(context.Background(), agent.AgentTypeCursor, cursorTranscript)
	assert.False(t, changed)
	assert.Equal(t, agent.AgentTypeCursor, corrected)
}

func TestCorrectSessionAgentType_NoTranscript_NoChange(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	corrected, changed := correctSessionAgentType(context.Background(), agent.AgentTypeClaudeCode, "")
	assert.False(t, changed)
	assert.Equal(t, agent.AgentTypeClaudeCode, corrected)
}

// TestInitializeSession_HintWinsRaceAtTurnStart simulates the bug:
// Cursor fires SessionStart first (wins the hint). Claude Code's TurnStart fires
// first (without transcript path). The session must still be recorded as Cursor.
func TestInitializeSession_HintWinsRaceAtTurnStart(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	ctx := context.Background()
	sessionID := "test-session-hint-race"

	// SessionStart phase: Cursor fires first, then Claude Code (no-op).
	created, err := StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeCursor)
	require.NoError(t, err)
	require.True(t, created)
	created, err = StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeClaudeCode)
	require.NoError(t, err)
	require.False(t, created)

	// TurnStart phase: Claude Code fires first with no transcript path (the bug condition).
	s := &ManualCommitStrategy{}
	err = s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "first prompt", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, agent.AgentTypeCursor, state.AgentType,
		"the SessionStart hint should claim the session for Cursor even when Claude Code's TurnStart fires first")
}

// TestInitializeSession_TranscriptPathRepairsExistingState simulates a session
// already recorded with the wrong AgentType. Cursor's TurnStart fires with a
// Cursor transcript path on a subsequent turn — state.AgentType must be repaired.
func TestInitializeSession_TranscriptPathRepairsExistingState(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	cursorTranscript := withCursorSessionDir(t)

	ctx := context.Background()
	sessionID := "test-session-repair"

	// Bootstrap an already-wrong session.
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "first prompt", ""))
	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, agent.AgentTypeClaudeCode, state.AgentType)

	// Subsequent turn arrives via Cursor's hook with a Cursor transcript path.
	err = s.InitializeSession(ctx, sessionID, agent.AgentTypeCursor, cursorTranscript, "second prompt", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, agent.AgentTypeCursor, state.AgentType,
		"a turn whose transcript path lives in Cursor's session dir must repair the wrong agent type")
	assert.Equal(t, cursorTranscript, state.TranscriptPath)
}

// TestInitializeSession_ConcurrentRaceRepairsOnNextTurn simulates the bug
// from the field: two processes (Cursor IDE forwarding the prompt to both
// .cursor/hooks.json and .claude/settings.json) call InitializeSession
// concurrently for the same session ID. We do not serialize them — the
// race may leave AgentType wrong for a single turn (both goroutines see
// no existing state and run the "initialize new session" path; last
// writer wins). The contract is eventual consistency: the next turn that
// arrives with the cursor transcript path repairs AgentType via
// correctSessionAgentType.
func TestInitializeSession_ConcurrentRaceRepairsOnNextTurn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	cursorTranscript := withCursorSessionDir(t)

	ctx := context.Background()
	sessionID := "test-session-concurrent"

	// Reproduce the SessionStart hint race where Claude Code happened to win.
	_, err := StoreAgentTypeHint(ctx, sessionID, agent.AgentTypeClaudeCode)
	require.NoError(t, err)

	// Two parallel hook processes: claude-code (no transcript) and cursor
	// (with transcript). Errors are non-fatal — without serialization the
	// goroutines may step on each other's writes; convergence is asserted
	// via the next-turn call below.
	s := &ManualCommitStrategy{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, "", "prompt", "") //nolint:errcheck // see comment above
	}()
	go func() {
		defer wg.Done()
		_ = s.InitializeSession(ctx, sessionID, agent.AgentTypeCursor, cursorTranscript, "prompt", "") //nolint:errcheck // see comment above
	}()
	wg.Wait()

	// Simulate the next turn: cursor's hook fires again with its transcript
	// path. correctSessionAgentType repairs any wrong AgentType from the race.
	require.NoError(t, s.InitializeSession(ctx, sessionID, agent.AgentTypeCursor, cursorTranscript, "next prompt", ""))

	state, lErr := s.loadSessionState(ctx, sessionID)
	require.NoError(t, lErr)
	require.NotNil(t, state)
	assert.Equal(t, agent.AgentTypeCursor, state.AgentType,
		"the transcript-bearing turn must repair AgentType to Cursor regardless of how the prior race resolved")
}

// TestInitializeSession_ClaudeCodeTranscriptKeepsClaudeCode verifies the negative case:
// a Claude Code transcript path doesn't get rewritten just because some other agent's
// hook also fired earlier.
func TestInitializeSession_ClaudeCodeTranscriptKeepsClaudeCode(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	claudeTranscript := withClaudeSessionDir(t)

	ctx := context.Background()
	sessionID := "test-session-claude"

	s := &ManualCommitStrategy{}
	err := s.InitializeSession(ctx, sessionID, agent.AgentTypeClaudeCode, claudeTranscript, "first prompt", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, agent.AgentTypeClaudeCode, state.AgentType)
}
