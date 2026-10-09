package cli

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// A hook that checkpoints while attach waits at its prompt moves the token
// offset; attach must not move it back to the position it counted to.
func TestSaveAttachSessionState_KeepsTokenOffsetMovedMeanwhile(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	ctx := context.Background()

	store, err := session.NewStateStore(ctx)
	require.NoError(t, err)
	const sessionID = "attach-window-session"
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	for _, tc := range []struct {
		name          string
		hookMovedTo   int
		wantStart     int
		wantPendingOK bool
	}{
		{name: "unchanged", hookMovedTo: 2, wantStart: 4},
		{name: "moved by a hook", hookMovedTo: 6, wantStart: 6, wantPendingOK: true},
	} {
		loaded := &session.State{SessionID: sessionID, AgentType: agent.AgentTypeClaudeCode}
		loaded.SetTokenStart(2)

		now := time.Now()
		onDisk := &session.State{
			SessionID:            sessionID,
			AgentType:            agent.AgentTypeClaudeCode,
			Phase:                session.PhaseActive,
			StartedAt:            now,
			LastInteractionTime:  &now,
			CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 3},
		}
		onDisk.SetTokenStart(tc.hookMovedTo)
		require.NoError(t, store.Save(ctx, onDisk))

		require.NoError(t, saveAttachSessionState(ctx, nil, loaded, sessionID, agent.AgentTypeClaudeCode,
			"/t.jsonl", "", cpID, transcriptMetadata{}, &agent.TokenUsage{OutputTokens: 9}, 4, 4, attachOptions{}, nil, false), tc.name)

		got, err := store.Load(ctx, sessionID)
		require.NoError(t, err)
		require.Equal(t, tc.wantStart, got.TokenStart(), tc.name)
		require.Equal(t, tc.wantPendingOK, got.CheckpointTokenUsage != nil, tc.name)
	}
}
