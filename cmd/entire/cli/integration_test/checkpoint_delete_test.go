//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// checkpointExistsLocally reports whether this clone holds the checkpoint: its
// per-checkpoint ref (git-refs) or its subtree on the local v1 branch.
func (env *TestEnv) checkpointExistsLocally(checkpointID string) bool {
	env.T.Helper()
	if env.usingGitRefs() {
		return anyRefNamed(env.T, env.RepoDir, checkpointID)
	}
	cmd := exec.CommandContext(env.T.Context(), "git", "cat-file", "-t", paths.MetadataBranchName+":"+CheckpointSummaryPath(checkpointID))
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// anyRefNamed reports whether dir has a checkpoint ref for id under any shard
// spelling.
func anyRefNamed(t *testing.T, dir, checkpointID string) bool {
	t.Helper()
	out := testutil.RunGit(t, dir, "for-each-ref", "--format=%(refname)", checkpointRefPrefix+"*/"+checkpointID)
	return strings.TrimSpace(out) != ""
}

// seedCheckpointCopy copies this clone's checkpoint onto another repository
// directly, standing in for a copy pushed there by an older CLI or another
// clone.
func (env *TestEnv) seedCheckpointCopy(target, checkpointID string) {
	env.T.Helper()
	refSpec := "refs/heads/" + paths.MetadataBranchName + ":refs/heads/" + paths.MetadataBranchName
	if env.usingGitRefs() {
		ref := checkpointRefName(checkpointID)
		refSpec = ref + ":" + ref
	}
	testutil.RunGit(env.T, env.RepoDir, "push", "--no-verify", "--force", target, refSpec)
}

func installRejectingPreReceive(t *testing.T, bareDir string) {
	t.Helper()
	hook := "#!/bin/sh\necho 'checkpoint deletes are blocked here' >&2\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(bareDir, "hooks", "pre-receive"), []byte(hook), 0o755))
}

func pushQueueContents(t *testing.T, env *TestEnv) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "entire-checkpoint-push-queue.jsonl"))
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}

// The whole lifecycle: a dry run changes nothing; the delete removes the local
// copy, the remote copy and the push-queue entry; neither the next push nor an
// amend of the commit that carried it brings it back; explain says it may have
// been deleted.
func TestCheckpointDelete_DeletesEverywhereAndStaysDeleted(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bare := env.SetupBareRemote()

		cpID := createCheckpointedCommit(t, env, "Add alpha", "alpha.go", "package alpha", "Add alpha")
		env.GitPushWithHooks("origin", "HEAD")
		require.True(t, env.CheckpointExistsOnRemote(bare, cpID), "fixture: checkpoint pushed")

		out := env.RunCLI("checkpoint", "delete", cpID, "--dry-run")
		assert.Contains(t, out, "Dry run: nothing was changed.")
		assert.Contains(t, out, "origin")
		require.True(t, env.checkpointExistsLocally(cpID), "a dry run must not delete locally")
		require.True(t, env.CheckpointExistsOnRemote(bare, cpID), "a dry run must not delete remotely")

		// The session that wrote it has not ended: refused without --force.
		out, err := env.RunCLIWithError("checkpoint", "delete", cpID)
		require.Error(t, err)
		assert.Contains(t, out, "is still active")
		var plan struct {
			SessionStates []struct {
				SessionID string `json:"session_id"`
			} `json:"session_states"`
		}
		require.NoError(t, json.Unmarshal([]byte(env.RunCLI("checkpoint", "delete", cpID, "--dry-run", "--json")), &plan))
		require.Len(t, plan.SessionStates, 1)
		require.NoError(t, env.SimulateSessionEnd(plan.SessionStates[0].SessionID))

		out, err = env.RunCLIWithError("checkpoint", "delete", cpID)
		require.Error(t, err, "without a TTY and without --force the delete is refused")
		assert.Contains(t, out, "refusing to delete checkpoint "+cpID+" without confirmation; pass --force")
		require.True(t, env.checkpointExistsLocally(cpID))

		out = env.RunCLI("checkpoint", "delete", cpID, "--force")
		assert.Contains(t, out, "Deleted checkpoint "+cpID)
		assert.False(t, env.checkpointExistsLocally(cpID), "local copy deleted")
		assert.False(t, env.CheckpointExistsOnRemote(bare, cpID), "remote copy deleted")
		assert.NotContains(t, pushQueueContents(t, env), cpID, "push queue entry removed")

		explainOut, err := env.RunCLIWithError("checkpoint", "explain", "--commit", env.GetHeadHash())
		require.Error(t, err)
		assert.Contains(t, explainOut, "checkpoint not found (deleted with `entire checkpoint delete`)")

		// The next push must not resurrect it.
		env.GitPushWithHooks("origin", "HEAD")
		assert.False(t, env.CheckpointExistsOnRemote(bare, cpID), "pre-push must not re-push a deleted checkpoint")

		// Amending the commit with its original message keeps nothing alive:
		// the deleted trailer is dropped and no session restores it.
		headMsg := env.GetCommitMessage(env.GetHeadHash())
		require.Contains(t, headMsg, cpID)
		env.GitCommitAmendWithShadowHooks(headMsg)
		assert.Empty(t, env.GetCheckpointIDFromCommitMessage(env.GetHeadHash()), "the amend must not carry the deleted trailer")
		env.GitPushWithHooks("origin", "+HEAD")
		assert.False(t, env.checkpointExistsLocally(cpID))
		assert.False(t, env.CheckpointExistsOnRemote(bare, cpID))
	})
}

// An amend that carries new agent work is linked to a fresh checkpoint, never
// to the deleted one.
func TestCheckpointDelete_AmendWithPendingWorkGetsFreshCheckpoint(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		env.SetupBareRemote()

		cpID := createCheckpointedCommit(t, env, "Add beta", "beta.go", "package beta", "Add beta")
		env.RunCLI("checkpoint", "delete", cpID, "--force")

		sess := env.NewSession()
		transcript := sess.CreateTranscript("Extend beta", []FileChange{{Path: "beta2.go", Content: "package beta\n"}})
		require.NoError(t, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "Extend beta", transcript))
		env.WriteFile("beta2.go", "package beta\n")
		require.NoError(t, env.SimulateStop(sess.ID, transcript))

		env.GitCommitAmendWithShadowHooks(env.GetCommitMessage(env.GetHeadHash()), "beta2.go")

		newID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
		require.NotEmpty(t, newID, "new agent work in the amend is linked to a checkpoint")
		assert.NotEqual(t, cpID, newID, "never the deleted checkpoint")
		assert.True(t, env.checkpointExistsLocally(newID))
		assert.False(t, env.checkpointExistsLocally(cpID))
	})
}

// With a dedicated checkpoint_remote, the store and a legacy copy on origin
// are both deleted, and both are really contacted.
func TestCheckpointDelete_CheckpointRemoteAndLegacyOrigin(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		ft := newForgeTransport(t, env)
		origin := ft.Bare(t, "github.com", "alice/app")
		store := ft.Bare(t, "github.com", "alice/app-checkpoints")
		addForgeRemote(t, env, "origin", "git@github.com:alice/app.git")
		setCheckpointRemote(t, env, paths.SettingsFileName, "github", "alice/app-checkpoints")

		cpID := createCheckpointedCommit(t, env, "Add gamma", "gamma.go", "package gamma", "Add gamma")
		pushWithHooksOutput(t, env, "origin")
		require.True(t, env.CheckpointExistsOnRemote(store, cpID), "fixture: store holds it")
		env.seedCheckpointCopy(origin, cpID)
		require.True(t, env.CheckpointExistsOnRemote(origin, cpID), "fixture: legacy origin copy")

		out := env.RunCLI("checkpoint", "delete", cpID, "--force")
		assert.Contains(t, out, "Deleted checkpoint "+cpID)
		assert.False(t, env.CheckpointExistsOnRemote(store, cpID), "checkpoint_remote copy deleted")
		assert.False(t, env.CheckpointExistsOnRemote(origin, cpID), "legacy origin copy deleted")
		contacted := ft.Contacted(t)
		assert.Contains(t, contacted, "git-receive-pack github.com/alice/app-checkpoints")
		assert.Contains(t, contacted, "git-receive-pack github.com/alice/app.git")
	})
}

// A remote whose pushurl differs from its fetch URL holds checkpoints at the
// push URL. The delete reaches it there; the fetch URL (which rejects every
// push) never held a copy, so it is not touched and the delete succeeds.
func TestCheckpointDelete_PushURLDiffersFromFetchURL(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		fetchBare := env.SetupBareRemote()
		pushBare := t.TempDir()
		testutil.RunGit(t, pushBare, "init", "--bare")
		testutil.RunGit(t, env.RepoDir, "remote", "set-url", "--push", "origin", pushBare)
		env.setGitConfigBaseline()
		env.GitPush("origin", "HEAD")

		cpID := createCheckpointedCommit(t, env, "Add delta", "delta.go", "package delta", "Add delta")
		env.GitPushWithHooks("origin", "HEAD")
		require.True(t, env.CheckpointExistsOnRemote(pushBare, cpID), "fixture: checkpoints land at the push URL")
		require.False(t, env.CheckpointExistsOnRemote(fetchBare, cpID))
		installRejectingPreReceive(t, fetchBare)

		env.RunCLI("checkpoint", "delete", cpID, "--force")
		assert.False(t, env.CheckpointExistsOnRemote(pushBare, cpID), "the push target is where the delete must land")
	})
}

// One remote refuses the delete: the others still lose their copy, the failure
// names a retry command, nothing resurrects the deleted copies, and the retry
// finishes the job once the remote allows it.
func TestCheckpointDelete_PartialFailureAndRetry(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		origin := env.SetupBareRemote()
		upstream := env.SetupNamedBareRemote("upstream")
		env.PatchSettings(map[string]any{"strategy_options": map[string]any{"checkpoint_push_remote": "upstream"}})

		cpID := createCheckpointedCommit(t, env, "Add eps", "eps.go", "package eps", "Add eps")
		env.RunPrePush("upstream")
		require.True(t, env.CheckpointExistsOnRemote(upstream, cpID))
		env.seedCheckpointCopy("origin", cpID)
		require.True(t, env.CheckpointExistsOnRemote(origin, cpID))
		installRejectingPreReceive(t, origin)

		out, err := env.RunCLIWithError("checkpoint", "delete", cpID, "--force")
		require.Error(t, err, "a failed remote fails the command")
		assert.Contains(t, out, "entire checkpoint delete "+cpID+" --remote origin")
		assert.False(t, env.checkpointExistsLocally(cpID), "the local copy is not restored")
		assert.False(t, env.CheckpointExistsOnRemote(upstream, cpID))
		assert.True(t, env.CheckpointExistsOnRemote(origin, cpID))

		env.RunPrePush("upstream")
		assert.False(t, env.CheckpointExistsOnRemote(upstream, cpID), "the next push must not resurrect it")

		require.NoError(t, os.Remove(filepath.Join(origin, "hooks", "pre-receive")))
		env.RunCLI("checkpoint", "delete", cpID, "--remote", "origin", "--force")
		assert.False(t, env.CheckpointExistsOnRemote(origin, cpID), "a remote-only retry deletes the leftover copy")
	})
}

// --local-only leaves every remote copy alone, including across the next
// push. On git-refs the local ref goes and pre-push has nothing to send; a
// read then refetches the remote copy and recreates the local ref. On
// git-branch the removal would be a local v1 commit that pre-push
// fast-forwards onto the remote, so the delete is refused before any write.
func TestCheckpointDelete_LocalOnlyLeavesRemotes(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bare := env.SetupBareRemote()

		cpID := createCheckpointedCommit(t, env, "Add zeta", "zeta.go", "package zeta", "Add zeta")
		env.GitPushWithHooks("origin", "HEAD")

		out, err := env.RunCLIWithError("checkpoint", "delete", cpID, "--local-only", "--force")
		if backend == StoreGitBranch {
			require.Error(t, err, "a local v1 removal would reach origin on the next push")
			assert.Contains(t, out, "which the next git push sends to origin")
			assert.Contains(t, out, "run without --local-only")
			assert.True(t, env.checkpointExistsLocally(cpID), "a refused delete writes nothing")
		} else {
			require.NoError(t, err, out)
			assert.False(t, env.checkpointExistsLocally(cpID))
		}

		env.RunPrePush("origin")
		assert.True(t, env.CheckpointExistsOnRemote(bare, cpID), "--local-only: the remote copy survives the next push")

		// Documented consequence: reads can still find the remote copy.
		explainOut, err := env.RunCLIWithError("checkpoint", "explain", "--checkpoint", cpID)
		require.NoError(t, err, explainOut)
		assert.Contains(t, explainOut, cpID)
		if backend == StoreGitRefs {
			assert.True(t, env.checkpointExistsLocally(cpID), "the read refetched the remote copy as a local ref")
		}
	})
}

// The same repository reached under two spellings (a push URL and a fetch URL
// that differ, as pushInsteadOf produces) is two targets. The second sees the
// checkpoint already gone: that is success, and the remote-tracking v1 ref
// still stops showing it.
func TestCheckpointDelete_SameRepositoryUnderTwoURLs(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bare := env.SetupBareRemote()
		cpID := createCheckpointedCommit(t, env, "Add mu", "mu.go", "package mu", "Add mu")
		env.GitPushWithHooks("origin", "HEAD")
		testutil.RunGit(t, env.RepoDir, "remote", "set-url", "--push", "origin", "file://"+bare)
		env.setGitConfigBaseline()
		tracking := "refs/remotes/origin/" + paths.MetadataBranchName
		if backend == StoreGitBranch {
			testutil.RunGit(t, env.RepoDir, "fetch", "origin", "+refs/heads/"+paths.MetadataBranchName+":"+tracking)
		}

		out, err := env.RunCLIWithError("checkpoint", "delete", cpID, "--force")
		require.NoError(t, err, "the same repository reached twice is not a failure:\n%s", out)
		assert.Contains(t, out, "Deleted checkpoint "+cpID+".")
		assert.False(t, env.CheckpointExistsOnRemote(bare, cpID))
		if backend == StoreGitBranch {
			trackingTree := testutil.RunGit(t, env.RepoDir, "ls-tree", "-r", "--name-only", tracking)
			assert.NotContains(t, trackingTree, CheckpointSummaryPath(cpID), "the tracking ref follows the removal")
		}
	})
}

// push_sessions=false stops automatic pushes, not a delete the user asked for.
func TestCheckpointDelete_PushSessionsDisabledStillDeletesRemote(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bare := env.SetupBareRemote()
		cpID := createCheckpointedCommit(t, env, "Add eta", "eta.go", "package eta", "Add eta")
		env.seedCheckpointCopy("origin", cpID)
		env.PatchSettings(map[string]any{"strategy_options": map[string]any{"push_sessions": false}})

		out := env.RunCLI("checkpoint", "delete", cpID, "--force")
		assert.Contains(t, out, "push_sessions is disabled")
		assert.False(t, env.CheckpointExistsOnRemote(bare, cpID))
	})
}

func TestCheckpointDelete_WithCheckpointTokenSet(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	env.ExtraEnv = append(env.ExtraEnv, "ENTIRE_CHECKPOINT_TOKEN=test-token")
	bare := env.SetupBareRemote()
	cpID := createCheckpointedCommit(t, env, "Add theta", "theta.go", "package theta", "Add theta")
	env.seedCheckpointCopy("origin", cpID)

	env.RunCLI("checkpoint", "delete", cpID, "--force")
	assert.False(t, env.CheckpointExistsOnRemote(bare, cpID))
}

// A ref stored under a differently-cased shard (what a case-insensitive
// filesystem produces) is found and deleted, locally and on the remote.
func TestCheckpointDelete_MixedCaseShard(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bare := env.SetupBareRemote()
	source := createCheckpointedCommit(t, env, "Add iota", "iota.go", "package iota", "Add iota")
	commit := strings.TrimSpace(testutil.RunGit(t, env.RepoDir, "rev-parse", checkpointRefName(source)))

	const ulid = "01K6ZQ2M8E3V7R5T9Y4X6W2A6B"
	lowerShardRef := checkpointRefPrefix + "6b/" + ulid
	testutil.RunGit(t, env.RepoDir, "update-ref", lowerShardRef, commit)
	testutil.RunGit(t, env.RepoDir, "push", "--no-verify", "origin", lowerShardRef+":"+lowerShardRef)

	env.RunCLI("checkpoint", "delete", ulid, "--force")
	assert.False(t, anyRefNamed(t, env.RepoDir, ulid))
	assert.False(t, anyRefNamed(t, bare, ulid))
}

// Only the remote-tracking v1 ref holds the checkpoint here (no local v1
// branch). The delete moves that ref past the removal, so a later
// migrate-checkpoints does not re-create the checkpoint as a ref.
func TestCheckpointDelete_MigrateAfterDeleteWithOnlyTrackingV1(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch
	bare := env.SetupBareRemote()
	cpID := createCheckpointedCommit(t, env, "Add kappa", "kappa.go", "package kappa", "Add kappa")
	env.GitPushWithHooks("origin", "HEAD")
	testutil.RunGit(t, env.RepoDir, "fetch", "origin", "+refs/heads/"+paths.MetadataBranchName+":refs/remotes/origin/"+paths.MetadataBranchName)
	testutil.RunGit(t, env.RepoDir, "update-ref", "-d", "refs/heads/"+paths.MetadataBranchName)

	env.RunCLI("checkpoint", "delete", cpID, "--force")
	assert.False(t, env.CheckpointExistsOnRemote(bare, cpID))
	tracking := testutil.RunGit(t, env.RepoDir, "ls-tree", "-r", "--name-only", "refs/remotes/origin/"+paths.MetadataBranchName)
	assert.NotContains(t, tracking, CheckpointSummaryPath(cpID), "the tracking ref follows the remote removal")

	env.RunCLI("doctor", "migrate-checkpoints")
	assert.False(t, anyRefNamed(t, env.RepoDir, cpID), "migration must not re-create a deleted checkpoint")
}

// After a git-branch delete reaches the remote, the next push still delivers
// new checkpoints: the delete commits the removal on the local v1 branch and on
// the remote's tip separately, so local v1 must end up descending from the tip
// the delete pushed. Otherwise the branches diverge, and with OPF enabled a
// diverged v1 aborts the user's push (V1DivergedError).
func TestCheckpointDelete_BranchBackendNextPushAfterRemoteDelete(t *testing.T) {
	t.Parallel()
	for _, unpushedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "local in sync", true: "local has an unpushed checkpoint"}[unpushedFirst], func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			env.CheckpointStore = StoreGitBranch
			bare := env.SetupBareRemote()
			cpID := createCheckpointedCommit(t, env, "Add nu", "nu.go", "package nu", "Add nu")
			env.GitPushWithHooks("origin", "HEAD")
			var unpushedID string
			if unpushedFirst {
				unpushedID = createCheckpointedCommit(t, env, "Add omicron", "omicron.go", "package omicron", "Add omicron")
			}

			env.RunCLI("checkpoint", "delete", cpID, "--force")
			require.False(t, env.CheckpointExistsOnRemote(bare, cpID))
			if unpushedFirst {
				assert.False(t, env.CheckpointExistsOnRemote(bare, unpushedID), "a delete never pushes unrelated local v1 commits")
			}
			remoteTip := strings.TrimSpace(testutil.RunGit(t, bare, "rev-parse", "refs/heads/"+paths.MetadataBranchName))
			testutil.RunGit(t, env.RepoDir, "merge-base", "--is-ancestor", remoteTip, "refs/heads/"+paths.MetadataBranchName)

			nextID := createCheckpointedCommit(t, env, "Add xi", "xi.go", "package xi", "Add xi")
			env.GitPushWithHooks("origin", "HEAD")
			assert.True(t, env.CheckpointExistsOnRemote(bare, nextID), "the next checkpoint still reaches the remote")
			if unpushedFirst {
				assert.True(t, env.CheckpointExistsOnRemote(bare, unpushedID))
			}
			assert.False(t, env.CheckpointExistsOnRemote(bare, cpID), "and the deleted one stays gone")
			assert.False(t, env.checkpointExistsLocally(cpID))
		})
	}
}
