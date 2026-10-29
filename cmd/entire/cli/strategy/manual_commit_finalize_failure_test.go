package strategy

import (
	"context"
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

func swapTranscriptPath(f *finalizeFixture, path string) (restore func()) {
	original := f.state.TranscriptPath
	f.state.TranscriptPath = path
	return func() { f.state.TranscriptPath = original }
}
