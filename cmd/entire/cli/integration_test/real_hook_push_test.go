//go:build integration

package integration

import (
	"testing"
)

// TestGitPushWithHooks_SyncsCheckpointsToRemote is the seed of test A1: a plain
// `git push` of a feature branch, running the installed pre-push hook exactly as
// git runs it (realistic stdin refspecs, remote name/URL argv), lands the
// committed checkpoints on the bare remote WITHOUT any explicit RunPrePush or
// PushCheckpointRefs. It runs under both checkpoint backends via ForEachBackend,
// validating the whole I-1/I-2 enabler stack: env injection selects the store,
// the real hook drains it, and the backend-aware assertion finds the result.
func TestGitPushWithHooks_SyncsCheckpointsToRemote(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupBareRemote()

		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		// Sanity: checkpoint exists locally under the selected backend.
		if !env.CheckpointsPresentLocally() {
			t.Fatalf("[%s] checkpoint should exist locally after condensation", backend)
		}

		// Plain push through the real hook — no explicit checkpoint push.
		env.GitPushWithHooks("origin", "HEAD")

		if !env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatalf("[%s] checkpoints should be on remote after `git push` via the real pre-push hook", backend)
		}
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Fatalf("[%s] checkpoint %s should be on remote after `git push` via the real pre-push hook", backend, checkpointID)
		}
	})
}
