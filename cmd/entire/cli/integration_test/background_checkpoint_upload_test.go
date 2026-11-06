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
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestGitPushWithHooks_CheckpointsLandAfterUserPush runs the hand-off end to
// end through the real binary: the pre-push hook pushes nothing itself, spawns
// the detached worker and returns, and the worker uploads the checkpoint only
// after the user's push has finished. GitHub rejects the later of two pushes
// that land back to back, so the order is what keeps the user's push safe.
func TestGitPushWithHooks_CheckpointsLandAfterUserPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()
	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}

	// Record the order in which the remote receives pushes.
	received := filepath.Join(t.TempDir(), "received")
	hook := "#!/bin/sh\nwhile read -r old new ref; do echo \"$ref\" >> '" + received + "'; done\nexit 0\n"
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
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push failed: %v\n%s", err, output)
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
	order, err := os.ReadFile(received)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(order))
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "refs/heads/") || !strings.HasPrefix(lines[len(lines)-1], "refs/entire/") {
		t.Fatalf("the user's branch must reach the remote before the checkpoint refs; received:\n%s", order)
	}
}

// TestRunPushThenUploadCheckpoints drives `entire trail create`'s push protocol
// through a real git push and the real pre-push hook: the hook only records the
// upload, the checkpoints go up in-process once the push returns, and a failed
// push discards its request with the refs left queued for the next push.
// Not parallel: the in-process upload resolves the repository from the CWD.
func TestRunPushThenUploadCheckpoints(t *testing.T) {
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()
	checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
	if checkpointID == "" {
		t.Fatal("should have a checkpoint ID after condensation")
	}
	env.InstallRealPrePushHook()
	for _, kv := range env.cliEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			t.Setenv(k, v)
		}
	}
	t.Chdir(env.RepoDir)
	paths.ClearWorktreeRootCache()
	coord := checkpoint.NewUploadCoordinator(filepath.Join(env.RepoDir, ".git"))

	// A push the remote refuses uploads nothing and leaves no request behind.
	reject := filepath.Join(bareDir, "hooks", "pre-receive")
	if err := os.WriteFile(reject, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	push := execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	push.Env = os.Environ()
	if err := strategy.RunPushThenUploadCheckpoints(t.Context(), push, "origin", &out); err == nil {
		t.Fatal("the refused push should fail")
	}
	if coord.HasRequest() {
		t.Fatal("a failed push must discard the upload request it recorded")
	}
	if env.CheckpointExistsOnRemote(bareDir, checkpointID) {
		t.Fatal("nothing uploads for a failed push")
	}

	// The same push accepted: the checkpoint is on the remote when it returns.
	if err := os.Remove(reject); err != nil {
		t.Fatal(err)
	}
	push = execx.NonInteractive(t.Context(), "git", "push", "origin", "HEAD")
	push.Env = os.Environ()
	if err := strategy.RunPushThenUploadCheckpoints(t.Context(), push, "origin", &out); err != nil {
		t.Fatalf("push failed: %v\n%s", err, out.String())
	}
	if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
		t.Fatalf("checkpoint %s should be on the remote once the push returns\n%s", checkpointID, out.String())
	}
	if coord.HasRequest() {
		t.Fatal("the request was consumed by the in-process upload")
	}
}
