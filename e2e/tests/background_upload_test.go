//go:build e2e

package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/e2e/testutil"
)

// TestGitPushUploadsCheckpointsInBackground leaves the background checkpoint
// upload on, the default for users, which the harness otherwise turns off for
// every other test: the pre-push hook hands the queue to a detached worker and
// returns, and the worker uploads the checkpoint after the push. The test waits
// for the worker's recorded result before checking the remote.
func TestGitPushUploadsCheckpointsInBackground(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if !testutil.UsingGitRefs() {
			t.Skip("the background upload is the git-refs pre-push path")
		}
		bareDir := testutil.SetupBareRemote(t, s)
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Enable entire")

		_, err := s.RunPrompt(t, ctx,
			"create a file at docs/background.md with one sentence about uploads. Do not ask for confirmation, just make the change.")
		require.NoError(t, err, "agent failed")
		s.Git(t, "add", ".")
		s.Git(t, "commit", "-m", "Add background doc")
		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		checkpointID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")

		push := exec.CommandContext(ctx, "git", "push", "origin", "HEAD")
		push.Dir = s.Dir
		push.Env = append(os.Environ(), "ENTIRE_CHECKPOINT_UPLOAD_FOREGROUND=",
			"CI=", "GITHUB_ACTIONS=", "GITLAB_CI=", "BUILDKITE=", "JENKINS_URL=", "TF_BUILD=")
		out, err := push.CombinedOutput()
		require.NoError(t, err, "git push failed: %s", out)
		require.Contains(t, string(out), "in the background", "the hook should hand off to the worker")

		commonDir := testutil.GitOutput(t, s.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
		coord := checkpoint.NewUploadCoordinator(filepath.Clean(commonDir))
		var last *checkpoint.UploadResult
		require.Eventually(t, func() bool {
			st, stErr := coord.State()
			if stErr != nil || st.Last == nil || st.Last.FinishedAt.IsZero() {
				return false
			}
			last = st.Last
			return true
		}, 90*time.Second, 200*time.Millisecond, "the background worker did not finish")
		require.Empty(t, last.Error, "the worker left refs queued")
		require.Positive(t, last.Pushed)

		// Let the worker release its lock and exit before temp dirs go away.
		require.Eventually(t, func() bool {
			release, ok, lockErr := coord.TryLockWorker(ctx)
			if lockErr != nil || !ok {
				return false
			}
			release()
			return true
		}, 30*time.Second, 100*time.Millisecond)

		remoteRefs := testutil.GitOutput(t, bareDir, "for-each-ref", "--format=%(refname)", "refs/entire/checkpoints/")
		require.Contains(t, remoteRefs, checkpointID,
			"checkpoint %s should be on the remote after the background upload; remote has:\n%s", checkpointID, remoteRefs)
	})
}
