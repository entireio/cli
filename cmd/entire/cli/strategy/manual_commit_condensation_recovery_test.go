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
		nil, nil, nil, nil, preservedBranches, nil, 0, nil,
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

// prepare-commit-msg reserves the stamped ID before the commit exists. A commit
// that never lands (editor aborted, commit-msg hook failed, trailer deleted)
// leaves that reservation with nothing written under it, and it must not stop
// the session condensing into the next commit's checkpoint. A reservation a
// condensation actually began still does.
func TestPreservesInterruptedCondensation(t *testing.T) {
	t.Parallel()
	reservedID := id.MustCheckpointID("111111111111")
	commitID := id.MustCheckpointID("222222222222")

	stamped := &SessionState{SessionID: "s"}
	stamped.ReserveStampedCheckpoint(reservedID)
	require.False(t, preservesInterruptedCondensation(stamped, commitID), "a stamped-only reservation holds nothing")

	begun := &SessionState{SessionID: "s"}
	begun.BeginCondensationAttempt(reservedID)
	require.True(t, preservesInterruptedCondensation(begun, commitID))
	require.False(t, preservesInterruptedCondensation(begun, reservedID), "the same ID resumes it")
	require.False(t, preservesInterruptedCondensation(&SessionState{SessionID: "s"}, commitID))
}

// A stamped reservation is only prepare-commit-msg's marker for its own
// commit; condensation attempts are separate and work as they always have.
// Across every reservation state and trailer: a commit is held back only by an
// ordinary attempt for another ID, and an eager or doctor condensation never
// writes under a stamped ID (no commit may reference it) nor leaves one behind
// that would hold later commits back.
func TestReservationStates(t *testing.T) {
	t.Parallel()
	x := id.MustCheckpointID("555555555555")
	y := id.MustCheckpointID("666666666666")
	states := map[string]func() *SessionState{
		"no attempt": func() *SessionState { return &SessionState{SessionID: "s"} },
		"stamped X": func() *SessionState {
			st := &SessionState{SessionID: "s"}
			st.ReserveStampedCheckpoint(x)
			return st
		},
		"attempt X": func() *SessionState {
			st := &SessionState{SessionID: "s"}
			st.BeginCondensationAttempt(x)
			return st
		},
	}
	for name, mk := range states {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, trailer := range []id.CheckpointID{x, y} {
				want := name == "attempt X" && trailer == y
				require.Equal(t, want, preservesInterruptedCondensation(mk(), trailer), "trailer %s", trailer)
			}

			st := mk()
			got, created, err := ensureCondensationAttemptID(context.Background(), st)
			require.NoError(t, err)
			require.Equal(t, got, st.PendingCondensationID())
			require.False(t, st.StampedReservationFor(got), "an eager/doctor attempt is ordinary")
			if name == "attempt X" {
				require.Equal(t, x, got)
				require.False(t, created)
			} else {
				require.NotEqual(t, x, got, "never writes under a stamped ID")
				require.True(t, created)
			}
		})
	}
}
