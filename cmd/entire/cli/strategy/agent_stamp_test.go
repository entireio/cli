package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file use t.Chdir (process-global state), so they cannot use
// t.Parallel().

func TestAgentStamps_RecordAndTake(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	ctx := context.Background()
	first, second := id.MustCheckpointID("a1b2c3d4e5f6"), id.MustCheckpointID("b2c3d4e5f6a1")

	recordAgentStamp(ctx, first, "sess-1")
	recordAgentStamp(ctx, second, "sess-2")

	assert.Equal(t, map[id.CheckpointID]string{first: "sess-1", second: "sess-2"}, takeAgentStamps(ctx))
	assert.Empty(t, takeAgentStamps(ctx), "taking consumes the record")
}

func TestAgentStamps_UnreadableRecordIsEmpty(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	testutil.WriteFile(t, filepath.Join(dir, ".git"), agentStampsFile, "not json")

	assert.Empty(t, takeAgentStamps(context.Background()))
}

func TestAgentStampedFor(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	ctx := withAgentStamp(context.Background(), cpID, "sess-1")

	assert.True(t, agentStampedFor(ctx, cpID, "sess-1"))
	assert.False(t, agentStampedFor(ctx, cpID, "sess-2"), "another session sharing the checkpoint")
	assert.False(t, agentStampedFor(ctx, id.MustCheckpointID("b2c3d4e5f6a1"), "sess-1"), "another checkpoint")
	assert.False(t, agentStampedFor(context.Background(), cpID, "sess-1"), "no stamp")
}

// A session whose transcript cannot be produced (here: the recorded path does
// not exist) and that recorded no files is written when the agent-commit fast
// path stamped the trailer for it, with the committed files; the commit
// already names its checkpoint.
func TestCondenseSession_AgentStampedSessionWithoutTranscript(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	s := &ManualCommitStrategy{}
	cpID := id.MustCheckpointID("c3d4e5f6a1b2")
	newState := func(sessionID string) *SessionState {
		return &SessionState{
			SessionID:  sessionID,
			AgentType:  agent.AgentTypeOpenCode,
			BaseCommit: getHeadHash(t, repo),
			// IDLE: an ACTIVE OpenCode session would run `opencode export`
			// to prepare the transcript.
			Phase:          session.PhaseIdle,
			TranscriptPath: filepath.Join(t.TempDir(), "missing.json"),
		}
	}
	committed := map[string]struct{}{"test.txt": {}}

	t.Run("stamped for this session", func(t *testing.T) {
		ctx := withAgentStamp(context.Background(), cpID, "stamped")
		result, err := s.CondenseSession(ctx, repo, cpID, newState("stamped"), committed)
		require.NoError(t, err)
		assert.False(t, result.Skipped)
		assert.Equal(t, []string{"test.txt"}, result.FilesTouched)
	})
	t.Run("stamped for another session", func(t *testing.T) {
		// The other session made the commit; this one has no evidence of
		// work and keeps today's outcome.
		ctx := withAgentStamp(context.Background(), cpID, "stamped")
		_, err := s.CondenseSession(ctx, repo, cpID, newState("bystander"), committed)
		require.ErrorContains(t, err, "live transcript is empty or unreadable")
	})
	t.Run("not stamped", func(t *testing.T) {
		_, err := s.CondenseSession(context.Background(), repo, cpID, newState("unstamped"), committed)
		require.ErrorContains(t, err, "live transcript is empty or unreadable")
	})
}

func TestFilterFilesTouched_AppliesFallbackForAgentStampedSession(t *testing.T) {
	t.Parallel()
	sessionData := &ExtractedSessionData{}
	state := &SessionState{SessionID: "stamped"}

	filterFilesTouched(sessionData, map[string]struct{}{"app/foo.go": {}}, state, true)

	assert.Equal(t, []string{"app/foo.go"}, sessionData.FilesTouched)
}

// A stamped checkpoint written without a transcript stored none of it, so the
// window stays where the last checkpoint left it: the next checkpoint covers
// the transcript this one could not, without re-covering earlier ones.
func TestAdvanceWindowAfterCondense(t *testing.T) {
	t.Parallel()
	cpID := id.MustCheckpointID("d4e5f6a1b2c3")
	stamped := withAgentStamp(context.Background(), cpID, "sess")
	for _, tt := range []struct {
		name      string
		ctx       context.Context
		lines     int
		wantStart int
	}{
		{"stamped, no transcript", stamped, 0, 7},
		{"stamped, transcript", stamped, 12, 12},
		{"not stamped, no transcript", context.Background(), 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := &SessionState{SessionID: "sess", CheckpointTranscriptStart: 7}
			advanceWindowAfterCondense(tt.ctx, state, &CondenseResult{CheckpointID: cpID, TotalTranscriptLines: tt.lines})
			assert.Equal(t, tt.wantStart, state.CheckpointTranscriptStart)
		})
	}
}
