package strategy

import (
	"context"
	"errors"
	"testing"

	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/redact"
	"github.com/stretchr/testify/require"
)

type interruptedRecoveryStore struct {
	cpkg.PersistentStore

	checkpointID id.CheckpointID
	sessionID    string
	failAt       string
}

type multiCandidateRecoveryStore struct {
	*interruptedRecoveryStore

	unreadableID id.CheckpointID
}

func (s *multiCandidateRecoveryStore) List(context.Context) ([]cpkg.CheckpointInfo, error) {
	return []cpkg.CheckpointInfo{
		{CheckpointID: s.unreadableID, SessionID: s.sessionID},
		{CheckpointID: s.checkpointID, SessionID: s.sessionID},
	}, nil
}

func (s *multiCandidateRecoveryStore) Read(ctx context.Context, checkpointID id.CheckpointID) (*cpkg.CheckpointSummary, error) {
	if checkpointID == s.unreadableID {
		return nil, errors.New("checkpoint read failed")
	}
	return s.interruptedRecoveryStore.Read(ctx, checkpointID)
}

func (s *interruptedRecoveryStore) List(context.Context) ([]cpkg.CheckpointInfo, error) {
	if s.failAt == "list" {
		return nil, errors.New("list failed")
	}
	return []cpkg.CheckpointInfo{{CheckpointID: s.checkpointID, SessionID: s.sessionID}}, nil
}

func (s *interruptedRecoveryStore) Read(context.Context, id.CheckpointID) (*cpkg.CheckpointSummary, error) {
	if s.failAt == "checkpoint" {
		return nil, errors.New("checkpoint read failed")
	}
	return &cpkg.CheckpointSummary{Sessions: make([]cpkg.SessionFilePaths, 1)}, nil
}

func (s *interruptedRecoveryStore) ReadSessionMetadata(context.Context, id.CheckpointID, int) (*cpkg.Metadata, error) {
	if s.failAt == "metadata" {
		return nil, errors.New("metadata read failed")
	}
	return &cpkg.Metadata{
		SessionID:                   s.sessionID,
		Strategy:                    StrategyNameManualCommit,
		CheckpointTranscriptStart:   2,
		TranscriptIdentifierAtStart: "transcript-start",
		CheckpointsCount:            1,
		SaveStepCount:               1,
	}, nil
}

func (s *interruptedRecoveryStore) ReadSessionContent(context.Context, id.CheckpointID, int) (*cpkg.SessionContent, error) {
	if s.failAt == "content" {
		return nil, errors.New("content read failed")
	}
	return &cpkg.SessionContent{Transcript: []byte("expected transcript")}, nil
}

func TestFindInterruptedCondensation_PropagatesIndeterminateReadErrors(t *testing.T) {
	t.Parallel()

	for _, failAt := range []string{"list", "checkpoint", "metadata", "content"} {
		t.Run(failAt, func(t *testing.T) {
			t.Parallel()
			state := &SessionState{
				SessionID:                   "interrupted-session",
				CheckpointTranscriptStart:   2,
				TranscriptIdentifierAtStart: "transcript-start",
				StepCount:                   1,
			}
			store := &interruptedRecoveryStore{
				checkpointID: id.MustCheckpointID("111111111111"),
				sessionID:    state.SessionID,
				failAt:       failAt,
			}

			_, _, err := findInterruptedCondensation(
				context.Background(), store, state,
				redact.AlreadyRedacted([]byte("expected transcript")), nil,
			)
			require.Error(t, err)
		})
	}
}

func TestFindInterruptedCondensation_ContinuesPastUnreadableCandidate(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		SessionID:                   "interrupted-session",
		CheckpointTranscriptStart:   2,
		TranscriptIdentifierAtStart: "transcript-start",
		StepCount:                   1,
	}
	matchingID := id.MustCheckpointID("222222222222")
	store := &multiCandidateRecoveryStore{
		interruptedRecoveryStore: &interruptedRecoveryStore{
			checkpointID: matchingID,
			sessionID:    state.SessionID,
		},
		unreadableID: id.MustCheckpointID("111111111111"),
	}

	checkpointID, found, err := findInterruptedCondensation(
		context.Background(), store, state,
		redact.AlreadyRedacted([]byte("expected transcript")), nil,
	)

	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, matchingID, checkpointID)
}

func TestPostCommitProcessSessionLocked_PreservesDifferentReservedAttempt(t *testing.T) {
	t.Parallel()

	reservedID := id.MustCheckpointID("111111111111")
	commitID := id.MustCheckpointID("222222222222")
	state := &SessionState{
		SessionID:  "interrupted-session",
		BaseCommit: "base-commit",
	}
	state.BeginCondensationAttempt(reservedID)
	preservedBranches := make(map[string]bool)

	(&ManualCommitStrategy{}).postCommitProcessSessionLocked(
		context.Background(), nil, state, nil, commitID, nil, nil, "", "",
		nil, nil, nil, nil, preservedBranches, nil, 0,
	)

	require.Equal(t, reservedID, state.PendingCondensationID())
	require.True(t, preservedBranches[getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)])
}

func TestReserveDoctorCondensationAttempt_PreservesLegacyRecoveryAcrossRetries(t *testing.T) {
	t.Parallel()

	state := &SessionState{
		SessionID: "legacy-interrupted-session",
		Phase:     session.PhaseEnded,
	}
	firstID, err := reserveDoctorCondensationAttempt(context.Background(), state)
	require.NoError(t, err)
	require.False(t, firstID.IsEmpty())
	require.True(t, state.NeedsCondensationRecovery())

	secondID, err := reserveDoctorCondensationAttempt(context.Background(), state)
	require.NoError(t, err)
	require.Equal(t, firstID, secondID)
	require.True(t, state.NeedsCondensationRecovery())
}
