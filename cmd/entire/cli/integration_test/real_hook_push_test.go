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

// TestGitPushWithHooks_DefersCheckpointsUntilFirstUserBranchExists ensures the
// user's own branch — not Entire metadata — is the first ref on a fresh remote.
//
// On the git-branch backend, entire/checkpoints/v1 is a real branch a forge
// could pick as the repository default, so its push is deferred until the
// user's branch has landed. On the git-refs backend, checkpoints live under
// refs/entire/*, which a forge cannot select as a default branch, so there is
// no hazard and they publish on the first push.
func TestGitPushWithHooks_DefersCheckpointsUntilFirstUserBranchExists(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupEmptyNamedBareRemote("origin")
		branch := env.GetCurrentBranch()
		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		// The first push must land the user's branch on the empty remote.
		env.GitPushWithHooks("origin", "HEAD")
		if !env.BranchExistsOnRemote(bareDir, branch) {
			t.Fatalf("[%s] first user branch %q should be on remote", backend, branch)
		}

		if backend == StoreGitRefs {
			// refs/entire/* can't become a default branch → no deferral.
			if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
				t.Fatalf("[git-refs] checkpoint %s should publish on the first push (no default-branch hazard)", checkpointID)
			}
			return
		}

		// git-branch: the v1 branch must be withheld until the user branch exists.
		if env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatalf("[git-branch] checkpoints must be deferred until after the first user branch push")
		}

		// The first push created a remote-tracking ref, so a later push publishes.
		env.WriteFile("later.go", "package later")
		env.GitAdd("later.go")
		env.GitCommit("Later user commit")
		env.GitPushWithHooks("origin", "HEAD")
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Fatalf("[git-branch] deferred checkpoint %s should be published on a later push", checkpointID)
		}
	})
}
