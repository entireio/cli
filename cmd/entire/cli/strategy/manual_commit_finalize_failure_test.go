package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// A finalize that fails before writing leaves every turn checkpoint with its
// provisional transcript. It must say so through capture degradation, and keep
// TurnCheckpointIDs so a repeated Stop in the same turn retries once the cause
// is gone; only turn start drops them.
func TestFinalizeAllTurnCheckpoints_EarlyFailureIsDegradation(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail breaks one finalize input and returns how to repair it.
		fail func(t *testing.T, f *finalizeFixture) (repair func())
	}{
		{
			name: "checkpoint_store_open_fails",
			fail: func(t *testing.T, _ *finalizeFixture) func() {
				t.Helper()
				// A backend this binary does not know, as a newer CLI could write.
				writeSettingsJSON(t, ".", `{"enabled": true, "checkpoints": {"primary": {"type": "git-refs"}, "mirrors": [{"type": "backend-from-a-newer-cli"}]}}`)
				return func() { writeGitRefsBackendSetting(t, ".") }
			},
		},
		{
			name: "no_transcript_path",
			fail: func(_ *testing.T, f *finalizeFixture) func() {
				return swapTranscriptPath(f, "")
			},
		},
		{
			name: "transcript_path_unresolvable",
			fail: func(t *testing.T, f *finalizeFixture) func() {
				t.Helper()
				return swapTranscriptPath(f, filepath.Join(t.TempDir(), "missing", paths.TranscriptFileName))
			},
		},
		{
			name: "transcript_unreadable",
			fail: func(t *testing.T, f *finalizeFixture) func() {
				t.Helper()
				dir := filepath.Join(t.TempDir(), paths.TranscriptFileName)
				require.NoError(t, os.Mkdir(dir, 0o700))
				return swapTranscriptPath(f, dir)
			},
		},
		{
			name: "transcript_empty",
			fail: func(t *testing.T, f *finalizeFixture) func() {
				t.Helper()
				empty := filepath.Join(t.TempDir(), paths.TranscriptFileName)
				require.NoError(t, os.WriteFile(empty, nil, 0o600))
				return swapTranscriptPath(f, empty)
			},
		},
		{
			name: "repository_open_fails",
			fail: func(t *testing.T, _ *finalizeFixture) func() {
				t.Helper()
				repoDir, err := os.Getwd()
				require.NoError(t, err)
				t.Chdir(t.TempDir())
				paths.ClearWorktreeRootCache()
				return func() {
					t.Chdir(repoDir)
					paths.ClearWorktreeRootCache()
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			turnIDs := append([]string(nil), f.state.TurnCheckpointIDs...)
			repair := tc.fail(t, f)

			s := NewManualCommitStrategy()
			s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }

			errCount := s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute))
			require.Equal(t, 1, errCount, "a failure, unlike an interruption, is still counted as an error")
			require.NotNil(t, f.state.CaptureDegradedAt, "checkpoints left provisional must show in `entire status`")
			require.Equal(t, turnIDs, f.state.TurnCheckpointIDs, "the IDs survive for a repeated Stop")

			repair()
			f.requireTranscripts(t, 0)

			// A repeated Stop retries the kept IDs and finishes the turn.
			require.Zero(t, s.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute)))
			require.Empty(t, f.state.TurnCheckpointIDs)
			f.requireTranscripts(t, len(f.checkpointIDs))
		})
	}
}

// A checkpoint whose own write fails keeps its provisional transcript while
// the rest of the turn finalizes. That is degraded capture too, and only the
// failed checkpoints are kept for a repeated Stop. An ID that is not a
// checkpoint ID can never be written, so it is reported but not kept.
func TestFinalizeAllTurnCheckpoints_WriteFailureIsDegradation(t *testing.T) {
	errRemote := errors.New("remote rejected the fetch")
	for _, tc := range []struct {
		name string
		// prefix is prepended to the fixture's turn IDs.
		prefix []string
		// fetch serves the call-th ref fetch (1-based); cancel ends the hook's context.
		fetch     func(f *finalizeFixture, cancel context.CancelFunc, call int, refName plumbing.ReferenceName) error
		errCount  int
		kept      []int
		finalized []bool
	}{
		{
			// The store skips every later fetch in the operation once one
			// fails, so the third checkpoint fails with the second.
			name: "fetch_fails_mid_turn",
			fetch: func(f *finalizeFixture, _ context.CancelFunc, call int, refName plumbing.ReferenceName) error {
				if call == 2 {
					return errRemote
				}
				return f.restore(refName)
			},
			errCount:  2,
			kept:      []int{1, 2},
			finalized: []bool{true, false, false},
		},
		{
			name:   "invalid_checkpoint_id",
			prefix: []string{"not-a-checkpoint-id"},
			fetch: func(f *finalizeFixture, _ context.CancelFunc, _ int, refName plumbing.ReferenceName) error {
				return f.restore(refName)
			},
			errCount:  1,
			finalized: []bool{true, true, true},
		},
		{
			name:   "failure_then_interrupted",
			prefix: []string{"not-a-checkpoint-id"},
			fetch: func(_ *finalizeFixture, cancel context.CancelFunc, _ int, _ plumbing.ReferenceName) error {
				cancel()
				return context.Canceled
			},
			errCount:  1,
			kept:      []int{0, 1, 2},
			finalized: []bool{false, false, false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFinalizeFixture(t)
			fixtureIDs := append([]string(nil), f.state.TurnCheckpointIDs...)
			f.state.TurnCheckpointIDs = append(append([]string(nil), tc.prefix...), fixtureIDs...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			s := NewManualCommitStrategy()
			calls := 0
			s.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error {
				calls++
				return tc.fetch(f, cancel, calls, refName)
			}

			errCount := s.finalizeAllTurnCheckpoints(ctx, f.state, time.Now().Add(time.Minute))
			require.Equal(t, tc.errCount, errCount, "failed checkpoints are still counted as errors")
			require.NotNil(t, f.state.CaptureDegradedAt, "a checkpoint left provisional must show in `entire status`")
			var kept []string
			for _, i := range tc.kept {
				kept = append(kept, fixtureIDs[i])
			}
			require.Equal(t, kept, f.state.TurnCheckpointIDs, "only checkpoints a retry can still write are kept")
			f.requireFinalized(t, tc.finalized)
			if len(kept) == 0 {
				return
			}

			// A repeated Stop retries the kept IDs and finishes the turn.
			retry := NewManualCommitStrategy()
			retry.checkpointRefFetcher = func(_ context.Context, refName plumbing.ReferenceName) error { return f.restore(refName) }
			require.Zero(t, retry.finalizeAllTurnCheckpoints(context.Background(), f.state, time.Now().Add(time.Minute)))
			require.Empty(t, f.state.TurnCheckpointIDs)
			f.requireTranscripts(t, len(f.checkpointIDs))
		})
	}
}

// requireFinalized checks each fixture checkpoint's transcript: finalized[i]
// says whether checkpoint i holds the full transcript or the provisional one.
func (f *finalizeFixture) requireFinalized(t *testing.T, finalized []bool) {
	t.Helper()
	f.restoreUnfetched(t)
	for i, checkpointID := range f.checkpointIDs {
		content, err := f.store.ReadSessionContent(context.Background(), checkpointID, 0)
		require.NoError(t, err)
		want := provisionalTranscript
		if finalized[i] {
			want = testTranscriptPromptResponse
		}
		require.Equal(t, want, string(content.Transcript), "checkpoint %d", i)
	}
}

func swapTranscriptPath(f *finalizeFixture, path string) (restore func()) {
	original := f.state.TranscriptPath
	f.state.TranscriptPath = path
	return func() { f.state.TranscriptPath = original }
}
