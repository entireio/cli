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
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
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
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	require.NoError(t, os.MkdirAll(filepath.Join(workDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"checkpoints":{"primary":{"type":"git-refs"}}}`),
		0o644,
	))

	repo, err := git.PlainOpen(workDir)
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

	transcriptPath := filepath.Join(workDir, ".entire", "metadata", sessionID, paths.TranscriptFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(testTranscriptPromptResponse), 0o644))
	state := &SessionState{
		SessionID:         sessionID,
		AgentType:         "Claude Code",
		TranscriptPath:    transcriptPath,
		TurnCheckpointIDs: []string{checkpointIDs[0].String(), checkpointIDs[1].String(), checkpointIDs[2].String()},
	}

	s := NewManualCommitStrategy()
	s.turnCheckpointFinalizeBudget = time.Second
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

	errCount := s.finalizeAllTurnCheckpoints(context.Background(), state)

	finalized := 0
	for _, checkpointID := range checkpointIDs {
		refName, refErr := checkpoint.RefName(checkpointID)
		require.NoError(t, refErr)
		if _, refErr = repo.Reference(refName, true); refErr == nil {
			finalized++
		}
	}

	require.Equal(t, 1, finalized, "only the fetch completed within the shared budget may finalize")
	require.Equal(t, len(checkpointIDs)-finalized, errCount, "every provisional checkpoint must count as an error")
	require.Equal(t, 2, fetchCalls, "no checkpoint fetch may start after the total budget expires")
	require.Empty(t, state.TurnCheckpointIDs, "a best-effort pass has no durable retry path")
	require.NotNil(t, state.CaptureDegradedAt, "the incomplete finalize must be visible through `entire status`")
}
