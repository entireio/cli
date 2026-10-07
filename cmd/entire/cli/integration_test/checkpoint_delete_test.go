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
		assert.Contains(t, explainOut, "checkpoint not found (may have been deleted)")

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

func TestCheckpointDelete_LocalOnlyLeavesRemotes(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend
		bare := env.SetupBareRemote()

		cpID := createCheckpointedCommit(t, env, "Add zeta", "zeta.go", "package zeta", "Add zeta")
		env.GitPushWithHooks("origin", "HEAD")

		env.RunCLI("checkpoint", "delete", cpID, "--local-only", "--force")
		assert.False(t, env.checkpointExistsLocally(cpID))
		assert.True(t, env.CheckpointExistsOnRemote(bare, cpID), "--local-only never touches a remote")

		// Documented consequence: reads may still find the remote copy.
		out, err := env.RunCLIWithError("checkpoint", "explain", "--checkpoint", cpID)
		t.Logf("explain after --local-only (err=%v):\n%s", err, out)
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
