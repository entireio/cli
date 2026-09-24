package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestTurnCheckpointFinalizeBudgetForAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agentType types.AgentType
		want      time.Duration
	}{
		{name: "codex", agentType: agent.AgentTypeCodex, want: 20 * time.Second},
		{name: "claude_code", agentType: agent.AgentTypeClaudeCode, want: 20 * time.Second},
		{name: "pi", agentType: agent.AgentTypePi, want: 5 * time.Second},
		{name: "other_supported_agent", agentType: agent.AgentTypeGemini, want: 5 * time.Second},
		{name: "unknown_agent", agentType: types.AgentType("future-agent"), want: 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, turnCheckpointFinalizeBudgetForAgent(tt.agentType))
		})
	}
}

func TestFinalizeAllTurnCheckpointsStopsAtTotalBudget(t *testing.T) {
	for _, tc := range []struct {
		name               string
		budget             time.Duration
		cancelParent       bool
		finalized, fetches int
		degraded           bool
	}{
		{name: "during_write", budget: time.Second, finalized: 1, fetches: 2, degraded: true},
		{name: "before_preparation", budget: -time.Second, finalized: 0, fetches: 0, degraded: true},
		{name: "canceled_parent", budget: time.Second, cancelParent: true, finalized: 0, fetches: 0},
		{name: "healthy", budget: 10 * time.Second, finalized: 3, fetches: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := setupGitRepo(t)
			t.Chdir(workDir)
			paths.ClearWorktreeRootCache()

			writeGitRefsBackendSetting(t, workDir)

			repo, err := gitrepo.OpenPath(workDir)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, repo.Close()) })

			sessionID := "slow-finalize"
			checkpointIDs := []id.CheckpointID{
				id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
				id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAW"),
				id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAX"),
			}
			stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
			require.NoError(t, err)
			refHashes := make(map[plumbing.ReferenceName]plumbing.Hash, len(checkpointIDs))
			for _, checkpointID := range checkpointIDs {
				require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
					CheckpointID: checkpointID,
					SessionID:    sessionID,
					Strategy:     StrategyNameManualCommit,
					Transcript:   redact.AlreadyRedacted([]byte("provisional transcript\n")),
					AuthorName:   "Test",
					AuthorEmail:  "test@example.com",
					Agent:        "Claude Code",
				}))
				refName, refErr := checkpoint.RefName(checkpointID)
				require.NoError(t, refErr)
				ref, refErr := repo.Reference(refName, true)
				require.NoError(t, refErr)
				refHashes[refName] = ref.Hash()
				require.NoError(t, repo.Storer.RemoveReference(refName))
			}

			_, err = checkpointpolicy.WriteLocal(context.Background(), repo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
			require.NoError(t, err)

			transcriptPath := filepath.Join(workDir, ".entire", "metadata", sessionID, paths.TranscriptFileName)
			require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
			require.NoError(t, os.WriteFile(transcriptPath, []byte(testTranscriptPromptResponse), 0o644))
			state := &SessionState{
				SessionID:         sessionID,
				StartedAt:         time.Now(),
				AgentType:         "Claude Code",
				TranscriptPath:    transcriptPath,
				TurnCheckpointIDs: []string{checkpointIDs[0].String(), checkpointIDs[1].String(), checkpointIDs[2].String()},
			}

			s := NewManualCommitStrategy()
			s.turnCheckpointFinalizeBudget = tc.budget
			fetchCalls := 0
			s.checkpointRefFetcher = func(ctx context.Context, refName plumbing.ReferenceName) error {
				fetchCalls++
				timer := time.NewTimer(600 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					return repo.Storer.SetReference(plumbing.NewHashReference(refName, refHashes[refName]))
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelParent {
				cancel()
			}
			require.NoError(t, SaveSessionState(context.Background(), state))
			var errCount int
			require.NoError(t, MutateSessionState(context.Background(), sessionID, func(current *SessionState) error {
				errCount = s.finalizeAllTurnCheckpoints(ctx, current)
				return nil
			}))
			state, err = LoadSessionState(context.Background(), sessionID)
			require.NoError(t, err)
			require.Equal(t, tc.degraded, state.CaptureDegradedAt != nil)
			require.Equal(t, len(checkpointIDs)-tc.finalized, errCount)
			require.Equal(t, tc.fetches, fetchCalls, "no fetch starts after cancellation")
			require.Empty(t, state.TurnCheckpointIDs)
			for i, checkpointID := range checkpointIDs {
				refName, refErr := checkpoint.RefName(checkpointID)
				require.NoError(t, refErr)
				if i >= tc.finalized {
					require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, refHashes[refName])))
				}
				content, readErr := stores.Persistent.ReadSessionContent(context.Background(), checkpointID, 0)
				require.NoError(t, readErr)
				if i < tc.finalized {
					require.Equal(t, []byte(testTranscriptPromptResponse), content.Transcript) //nolint:testifylint // Finalization must preserve transcript bytes exactly.
				} else {
					require.Equal(t, "provisional transcript\n", string(content.Transcript))
				}
			}
			// A fresh turn-end can acquire state and rewrite every provisional checkpoint.
			s.turnCheckpointFinalizeBudget = 10 * time.Second
			require.NoError(t, MutateSessionState(context.Background(), sessionID, func(current *SessionState) error {
				current.TurnCheckpointIDs = []string{checkpointIDs[0].String(), checkpointIDs[1].String(), checkpointIDs[2].String()}
				return s.HandleTurnEnd(context.Background(), current)
			}))
			for _, checkpointID := range checkpointIDs {
				content, readErr := stores.Persistent.ReadSessionContent(context.Background(), checkpointID, 0)
				require.NoError(t, readErr)
				require.Equal(t, []byte(testTranscriptPromptResponse), content.Transcript) //nolint:testifylint // Finalization must preserve transcript bytes exactly.
			}
		})
	}
}
