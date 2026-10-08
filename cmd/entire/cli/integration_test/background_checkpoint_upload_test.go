//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestGitPushWithHooks_BackgroundUploadFinishesSlowPush runs the hybrid end to
// end through the real binary: the pre-push hook's inline upload is cut by its
// budget on a slow remote, the hook spawns the detached worker and returns, and
// the worker lands the checkpoint after the user's push has finished.
func TestGitPushWithHooks_BackgroundUploadFinishesSlowPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()
	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}

	// The first push carrying checkpoint refs outlasts the inline budget; every
	// later push (the user's branch, the worker's upload) is immediate.
	marker := filepath.Join(t.TempDir(), "slowed")
	hook := "#!/bin/sh\nif grep -q refs/entire/ && [ ! -e '" + marker + "' ]; then touch '" + marker + "'; sleep 30; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	env.InstallRealPrePushHook()
	cmd := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	cmd.Dir = env.RepoDir
	// Opt this one push back into the hand-off the harness disables, and out
	// of the CI detection a CI runner would trip.
	cmd.Env = append(env.cliEnv(), strategy.CheckpointUploadForegroundEnv+"=",
		"CI=", "GITHUB_ACTIONS=", "GITLAB_CI=", "BUILDKITE=", "JENKINS_URL=", "TF_BUILD=")
	start := time.Now()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push failed: %v\n%s", err, output)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("git push took %s; the hook must hand off instead of waiting out the slow remote", elapsed)
	}
	if !strings.Contains(string(output), "in the background") {
		t.Fatalf("hook did not hand off to the background worker:\n%s", output)
	}

	// Wait for the worker to finish before the test's temp dirs go away.
	commonDir := filepath.Join(env.RepoDir, ".git")
	coord := checkpoint.NewUploadCoordinator(commonDir)
	deadline := time.Now().Add(90 * time.Second)
	var last *checkpoint.UploadResult
	for time.Now().Before(deadline) {
		if st, stErr := coord.State(); stErr == nil && st.Last != nil && !st.Last.FinishedAt.IsZero() {
			last = st.Last
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if last == nil {
		t.Fatal("background worker did not finish within 90s")
	}
	// The worker still releases its lock and logs after recording the run;
	// let it exit before the test's temp dirs are removed.
	for time.Now().Before(deadline) {
		release, ok, lockErr := coord.TryLockWorker(t.Context())
		if lockErr == nil && ok {
			release()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if last.Error != "" {
		t.Fatalf("background worker left refs queued: %s", last.Error)
	}
	if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
		t.Fatalf("checkpoint %s should be on the remote after the background upload", checkpointID)
	}
}
