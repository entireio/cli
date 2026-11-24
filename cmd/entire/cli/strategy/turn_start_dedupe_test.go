package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// A second turn-start for the prompt the running turn started from (a
// user-level hook firing beside a project one) is refused without touching
// the turn; a different prompt, a different agent turn ID, a turn that has
// ended, or a repeat past the window each start a new turn.
func TestInitializeSessionForPrompt_SkipsARepeatOfTheRunningTurn(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	const sessionID = "dedupe-session"
	start := func(prompt, agentTurnID string) error {
		return s.InitializeSessionForPrompt(ctx, sessionID, agent.AgentTypeClaudeCode, "", prompt, "", agentTurnID)
	}
	turnID := func() string {
		state, err := s.loadSessionState(ctx, sessionID)
		require.NoError(t, err)
		return state.TurnID
	}

	require.NoError(t, start("fix the bug", ""))
	first := turnID()
	require.ErrorIs(t, start("fix the bug", ""), ErrDuplicateTurnStart)
	assert.Equal(t, first, turnID(), "the repeat leaves the running turn alone")

	require.NoError(t, start("fix the bug", "turn-2"), "a new agent turn ID is a new prompt")
	second := turnID()
	assert.NotEqual(t, first, second)

	require.NoError(t, start("another prompt", ""))
	third := turnID()

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	require.NoError(t, s.saveSessionState(ctx, state))
	require.NoError(t, start("another prompt", ""), "the same prompt after its turn ended is a new turn")
	fourth := turnID()
	assert.NotEqual(t, third, fourth)

	state, err = s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	old := time.Now().Add(-2 * turnStartDedupeWindow)
	state.LastTurnStartAt = &old
	require.NoError(t, s.saveSessionState(ctx, state))
	require.NoError(t, start("another prompt", ""), "a repeat past the window is a new turn")
	assert.NotEqual(t, fourth, turnID())
}

// Plain InitializeSession never dedupes, and the key stores only a hash.
func TestInitializeSession_DoesNotDedupe(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	require.NoError(t, s.InitializeSession(ctx, "plain", agent.AgentTypeClaudeCode, "", "same", ""))
	require.NoError(t, s.InitializeSession(ctx, "plain", agent.AgentTypeClaudeCode, "", "same", ""))

	assert.Empty(t, TurnStartKey("s", "", ""), "nothing to tell prompts apart by")
	key := TurnStartKey("s", "secret prompt", "")
	assert.Len(t, key, 64)
	assert.NotContains(t, key, "secret")
}
