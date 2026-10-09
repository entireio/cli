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
// offset and records newer totals; attach must not move the offset back or
// overwrite those totals with the older ones it computed before the wait.
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
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")

	for _, tc := range []struct {
		name string
		// hookMovedTo is the token offset on disk when attach saves; -1 means
		// the session had no state at all.
		hookMovedTo   int
		wantStart     int
		wantPendingOK bool
		wantTotal     int
	}{
		{name: "unchanged", hookMovedTo: 2, wantStart: 4, wantTotal: 9},
		{name: "moved by a hook", hookMovedTo: 6, wantStart: 6, wantPendingOK: true, wantTotal: 12},
		{name: "no earlier state", hookMovedTo: -1, wantStart: 4, wantTotal: 9},
	} {
		sessionID := "attach-window-" + map[int]string{2: "unchanged", 6: "moved", -1: "new"}[tc.hookMovedTo]
		var loaded *session.State
		if tc.hookMovedTo >= 0 {
			loaded = &session.State{SessionID: sessionID, AgentType: agent.AgentTypeClaudeCode}
			loaded.SetTokenStart(2)

			now := time.Now()
			onDisk := &session.State{
				SessionID:            sessionID,
				AgentType:            agent.AgentTypeClaudeCode,
				Phase:                session.PhaseActive,
				StartedAt:            now,
				LastInteractionTime:  &now,
				TokenUsage:           &agent.TokenUsage{OutputTokens: 12},
				CheckpointTokenUsage: &agent.TokenUsage{OutputTokens: 3},
			}
			onDisk.SetTokenStart(tc.hookMovedTo)
			require.NoError(t, store.Save(ctx, onDisk))
		}

		// transcriptEnd 7 moves an ended session's display offset, which a
		// state without its own token offset would otherwise be read through.
		require.NoError(t, saveAttachSessionState(ctx, nil, loaded, sessionID, agent.AgentTypeClaudeCode,
			"/t.jsonl", "", cpID, transcriptMetadata{}, &agent.TokenUsage{OutputTokens: 9}, 4, 7, attachOptions{}, nil, false), tc.name)

		got, err := store.Load(ctx, sessionID)
		require.NoError(t, err)
		require.Equal(t, tc.wantStart, got.TokenStart(), tc.name)
		require.Equal(t, tc.wantPendingOK, got.CheckpointTokenUsage != nil, tc.name)
		require.NotNil(t, got.TokenUsage, tc.name)
		require.Equal(t, tc.wantTotal, got.TokenUsage.OutputTokens, tc.name)
	}
}
