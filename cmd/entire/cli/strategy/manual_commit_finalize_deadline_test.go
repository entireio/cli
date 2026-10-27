package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

const provisionalTranscript = "provisional transcript\n"

// finalizeFixture is a repository holding three provisional turn checkpoints
// whose refs exist only "on the remote": each ref is removed locally and comes
// back only when the injected fetcher restores it.
type finalizeFixture struct {
	repo          *git.Repository
	store         checkpoint.PersistentStore
	checkpointIDs []id.CheckpointID
	refHashes     map[plumbing.ReferenceName]plumbing.Hash
	state         *SessionState
}

func newFinalizeFixture(t *testing.T) *finalizeFixture {
	t.Helper()
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()
	writeGitRefsBackendSetting(t, workDir)

	repo, err := gitrepo.OpenPath(workDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })

	sessionID := "slow-finalize"
	f := &finalizeFixture{
		repo: repo,
		checkpointIDs: []id.CheckpointID{
			id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
			id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAW"),
			id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAX"),
		},
		refHashes: make(map[plumbing.ReferenceName]plumbing.Hash),
	}
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	f.store = stores.Persistent

	turnIDs := make([]string, 0, len(f.checkpointIDs))
	for _, checkpointID := range f.checkpointIDs {
		require.NoError(t, f.store.Write(context.Background(), checkpoint.Session{
			CheckpointID: checkpointID,
			SessionID:    sessionID,
			Strategy:     StrategyNameManualCommit,
			Transcript:   redact.AlreadyRedacted([]byte(provisionalTranscript)),
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
			Agent:        "Claude Code",
		}))
		refName, refErr := checkpoint.RefName(checkpointID)
		require.NoError(t, refErr)
		ref, refErr := repo.Reference(refName, true)
		require.NoError(t, refErr)
		f.refHashes[refName] = ref.Hash()
		require.NoError(t, repo.Storer.RemoveReference(refName))
		turnIDs = append(turnIDs, checkpointID.String())
	}

	transcriptPath := filepath.Join(workDir, ".entire", "metadata", sessionID, paths.TranscriptFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(testTranscriptPromptResponse), 0o644))
	f.state = &SessionState{
		SessionID:         sessionID,
		StartedAt:         time.Now(),
		AgentType:         "Claude Code",
		TranscriptPath:    transcriptPath,
		TurnCheckpointIDs: turnIDs,
	}
	return f
}

// restore puts a checkpoint ref back, as a successful fetch would.
func (f *finalizeFixture) restore(refName plumbing.ReferenceName) error {
	return f.repo.Storer.SetReference(plumbing.NewHashReference(refName, f.refHashes[refName]))
}

// restoreUnfetched makes the checkpoints finalize never fetched readable, so
// their stored transcripts can be inspected. A ref that exists locally is left
// alone: it may already point at the finalized commit.
func (f *finalizeFixture) restoreUnfetched(t *testing.T) {
	t.Helper()
	for refName := range f.refHashes {
		if _, err := f.repo.Reference(refName, false); err == nil {
			continue
		}
		require.NoError(t, f.restore(refName))
	}
}

func (f *finalizeFixture) requireTranscripts(t *testing.T, finalized int) {
	t.Helper()
	f.restoreUnfetched(t)
	for i, checkpointID := range f.checkpointIDs {
		content, err := f.store.ReadSessionContent(context.Background(), checkpointID, 0)
		require.NoError(t, err)
		if i < finalized {
			require.Equal(t, []byte(testTranscriptPromptResponse), content.Transcript, "checkpoint %d should be finalized", i) //nolint:testifylint // Finalization must preserve transcript bytes exactly.
		} else {
			require.Equal(t, provisionalTranscript, string(content.Transcript), "checkpoint %d should stay provisional", i)
		}
	}
}

func TestFinalizeAllTurnCheckpoints_Deadline(t *testing.T) {
	type fetchFunc func(f *finalizeFixture, cancelParent context.CancelFunc, call int) checkpoint.RefFetchFunc

	immediate := func(f *finalizeFixture, _ context.CancelFunc, _ int) checkpoint.RefFetchFunc {
		return func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }
	}
	// The first fetch succeeds; the second ends the parent context and then
	// blocks until finalize observes it, like a host signalling the hook
	// mid-loop. Nothing depends on wall-clock timing.
	cancelOnSecond := func(f *finalizeFixture, cancelParent context.CancelFunc, call int) checkpoint.RefFetchFunc {
		return func(ctx context.Context, refName plumbing.ReferenceName) error {
			if call == 1 {
				return f.restore(refName)
			}
			cancelParent()
			<-ctx.Done()
			return context.Cause(ctx)
		}
	}
	// The first fetch succeeds; the second hangs like a stalled remote until
	// the turn-end deadline ends it. The fallback fails the test instead of
	// hanging it if finalize stops honouring the deadline.
	stallOnSecond := func(f *finalizeFixture, _ context.CancelFunc, call int) checkpoint.RefFetchFunc {
		return func(ctx context.Context, refName plumbing.ReferenceName) error {
			if call == 1 {
				return f.restore(refName)
			}
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case <-time.After(10 * time.Second):
				return errors.New("fetch outlived the turn-end deadline")
			}
		}
	}
	// A slow-but-alive remote: every fetch succeeds after a delay.
	slow := func(f *finalizeFixture, _ context.CancelFunc, _ int) checkpoint.RefFetchFunc {
		return func(_ context.Context, refName plumbing.ReferenceName) error {
			time.Sleep(50 * time.Millisecond)
			return f.restore(refName)
		}
	}

	for _, tc := range []struct {
		name         string
		deadline     func() time.Time
		cancelBefore bool
		fetch        fetchFunc
		finalized    int
		fetches      int
		degraded     bool
		// cause is what the interrupted fetch saw end its context, so logs
		// can tell the budget apart from the host cancelling the hook.
		cause error
	}{
		{name: "no_deadline", deadline: noFinalizeDeadline, fetch: immediate, finalized: 3, fetches: 3},
		{name: "future_deadline", deadline: func() time.Time { return time.Now().Add(time.Minute) }, fetch: immediate, finalized: 3, fetches: 3},
		{name: "slow_fetches_without_deadline", deadline: noFinalizeDeadline, fetch: slow, finalized: 3, fetches: 3},
		{name: "deadline_passed_before_start", deadline: func() time.Time { return time.Now().Add(-time.Second) }, fetch: immediate, finalized: 0, fetches: 0, degraded: true},
		// The deadline also covers transcript preparation and the store open,
		// so it leaves room for a slow runner to reach the first fetch.
		{name: "deadline_expires_mid_loop", deadline: func() time.Time { return time.Now().Add(3 * time.Second) }, fetch: stallOnSecond, finalized: 1, fetches: 2, degraded: true, cause: errTurnEndDeadline},
		{name: "context_ends_mid_loop", deadline: noFinalizeDeadline, fetch: cancelOnSecond, finalized: 1, fetches: 2, degraded: true, cause: context.Canceled},
		// The host's budget still bounds the hook while the deadline does: a
		// parent cancellation under a live deadline is not reported as the budget.
		{name: "context_ends_mid_loop_under_deadline", deadline: func() time.Time { return time.Now().Add(time.Minute) }, fetch: cancelOnSecond, finalized: 1, fetches: 2, degraded: true, cause: context.Canceled},
		{name: "parent_canceled_before_start", deadline: noFinalizeDeadline, cancelBefore: true, fetch: immediate, finalized: 0, fetches: 0, degraded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}

			s := NewManualCommitStrategy()
			fetches := 0
			var fetchErrs []error
			s.checkpointRefFetcher = func(fetchCtx context.Context, refName plumbing.ReferenceName) error {
				fetches++
				err := tc.fetch(f, cancel, fetches)(fetchCtx, refName)
				if err != nil {
					fetchErrs = append(fetchErrs, err)
				}
				return err
			}

			require.NoError(t, SaveSessionState(context.Background(), f.state))
			var errCount int
			require.NoError(t, MutateSessionState(context.Background(), f.state.SessionID, func(current *SessionState) error {
				errCount = s.finalizeAllTurnCheckpoints(ctx, current, tc.deadline())
				return nil
			}))

			state, err := LoadSessionState(context.Background(), f.state.SessionID)
			require.NoError(t, err)
			require.Equal(t, tc.degraded, state.CaptureDegradedAt != nil, "capture degradation")
			require.Zero(t, errCount, "an interruption is reported as capture degradation, not as errors")
			require.Equal(t, tc.fetches, fetches, "no fetch may start once the context has ended")
			if tc.cause != nil {
				require.Len(t, fetchErrs, 1)
				require.ErrorIs(t, fetchErrs[0], tc.cause, "the interrupted fetch must see why its context ended")
			}
			var unfinalized []string
			if tc.degraded {
				unfinalized = f.state.TurnCheckpointIDs[tc.finalized:]
			}
			require.Equal(t, unfinalized, state.TurnCheckpointIDs, "an interruption keeps the unfinalized IDs for a repeated Stop")
			f.requireTranscripts(t, tc.finalized)
			if !tc.degraded {
				return
			}

			// A repeated Stop retries the kept IDs and finishes the turn.
			retry := NewManualCommitStrategy()
			retry.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }
			require.Zero(t, retry.finalizeAllTurnCheckpoints(context.Background(), state, noFinalizeDeadline()))
			require.Empty(t, state.TurnCheckpointIDs)
			f.requireTranscripts(t, len(f.checkpointIDs))
		})
	}
}

// An ordinary per-checkpoint failure is an error, not a timeout: it must not
// raise the capture-degraded warning.
func TestFinalizeAllTurnCheckpoints_WriteErrorIsNotDegradation(t *testing.T) {
	f := newFinalizeFixture(t)
	f.state.TurnCheckpointIDs = append([]string{"not-a-checkpoint-id"}, f.state.TurnCheckpointIDs...)

	s := NewManualCommitStrategy()
	s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }

	errCount := s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute))
	require.Equal(t, 1, errCount)
	require.Nil(t, f.state.CaptureDegradedAt)
	f.requireTranscripts(t, len(f.checkpointIDs))
}

// Once the deadline has passed, nothing finalize prepares can be written, so
// it must not start the local preparation at all: that time is the headroom
// the hook needs to save state before its host kills it. The transcript path
// does not exist, so preparation, had it started, would have cleared the IDs
// and reported an error.
func TestFinalizeAllTurnCheckpoints_ExpiredDeadlineSkipsPreparation(t *testing.T) {
	t.Parallel()

	turnIDs := []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW"}
	state := &SessionState{
		SessionID:         "expired-before-finalize",
		AgentType:         "Claude Code",
		TranscriptPath:    filepath.Join(t.TempDir(), "missing", paths.TranscriptFileName),
		TurnCheckpointIDs: append([]string(nil), turnIDs...),
	}

	errCount := NewManualCommitStrategy().finalizeAllTurnCheckpoints(context.Background(), state, time.Now().Add(-time.Second))
	require.Zero(t, errCount, "an interruption is reported as capture degradation, not as errors")
	require.NotNil(t, state.CaptureDegradedAt)
	require.Equal(t, turnIDs, state.TurnCheckpointIDs, "the IDs survive for a repeated Stop")
}

func noFinalizeDeadline() time.Time { return time.Time{} }
