//go:build integration

package integration

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// D1 -- git-branch divergence & recovery matrix
//
// Systematizes the local-v1 state {ahead, behind, diverged, disconnected,
// missing} x trigger {pre-push, explain fetch-on-miss, doctor} matrix that the
// #953/#1251/#1252/#1260 fixes patched piecemeal. The reconcile engine itself
// (SafelyAdvanceLocalRef, ReconcileDisconnectedMetadataRef, cherryPickOnto) is
// unit-tested exhaustively in the strategy package (safely_advance_local_ref_test.go,
// metadata_reconcile_test.go); these tests exercise it end-to-end through the
// real CLI triggers, where the historical regressions actually lived (wiring).
//
// git-branch only: assertions read the single v1 branch tree and its tip parent
// count. The git-refs per-checkpoint-ref divergence matrix is D2/D3.
//
// Invariants across the matrix:
//   - local-only commits always survive;
//   - a diverged push replays local onto remote, preserving BOTH sides;
//   - the replay is linear (tip has one parent), and a repeated trigger is
//     idempotent -- the double-replay guard for #1260.
// =============================================================================

// advanceRemoteV1 clones bareDir, runs a real checkpointed session on a throwaway
// branch, and pushes its v1 through the pre-push hook so the remote v1 branch
// advances by one checkpoint as if another machine had pushed. Returns the new
// remote-only checkpoint ID. The prompt is embedded in the checkpoint so callers
// can assert on it after a fetch-on-miss read.
func advanceRemoteV1(t *testing.T, srcEnv *TestEnv, bareDir, prompt string) string {
	t.Helper()

	other := srcEnv.CloneFrom(bareDir)
	other.GitCheckoutNewBranch("feature/remote-advance")
	cp := createCheckpointedCommit(t, other, prompt, "remote_adv.go", "package remoteadv", prompt)
	require.NotEmpty(t, cp, "remote-advance clone should produce a checkpoint")
	// Push only the checkpoint refs (not the throwaway branch) to advance remote v1.
	other.RunPrePush("origin")
	require.True(t, srcEnv.CheckpointExistsOnRemote(bareDir, cp),
		"remote v1 should carry the advanced checkpoint after the clone pushed")
	return cp
}

// forceLocalV1Orphan replaces the local entire/checkpoints/v1 branch with an
// orphan commit (no shared history with the remote) carrying a single checkpoint
// shard. This reproduces the empty-orphan-bug artifact: a local v1 that is
// disconnected from the remote v1. It builds the commit with plumbing + a
// temporary index so the working tree and current branch are untouched.
func forceLocalV1Orphan(t *testing.T, env *TestEnv, checkpointID string) {
	t.Helper()

	metaPath := CheckpointSummaryPath(checkpointID)
	content := `{"checkpoint_id":"` + checkpointID + `"}`

	runGit := func(stdin string, args ...string) string {
		c := exec.CommandContext(env.T.Context(), "git", args...)
		c.Dir = env.RepoDir
		c.Env = append(testutil.GitIsolatedEnv(), "GIT_INDEX_FILE="+filepath.Join(env.RepoDir, ".git", "entire-orphan-index"))
		if stdin != "" {
			c.Stdin = strings.NewReader(stdin)
		}
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v failed: %s", args, out)
		return strings.TrimSpace(string(out))
	}

	blobSHA := runGit(content, "hash-object", "-w", "--stdin")
	runGit("", "read-tree", "--empty")
	runGit("", "update-index", "--add", "--cacheinfo", "100644,"+blobSHA+","+metaPath)
	treeSHA := runGit("", "write-tree")
	commitSHA := runGit("", "commit-tree", treeSHA, "-m", "Checkpoint: "+checkpointID)
	runGit("", "update-ref", plumbingBranchRef(paths.MetadataBranchName), commitSHA)
}

func plumbingBranchRef(shortName string) string { return "refs/heads/" + shortName }

// TestDivergedV1_PrePushMatrix drives the real pre-push hook against each local-v1
// divergence state and asserts the invariants. It stays on the default git-branch
// backend (D1 is git-branch).
func TestDivergedV1_PrePushMatrix(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"ahead", "behind", "diverged", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()

			env := NewFeatureBranchEnv(t)
			bareDir := env.SetupBareRemote()

			// Shared base: one checkpoint pushed so local and remote v1 align.
			baseCP := createCheckpointedCommit(t, env, "base work", "base.go", "package base", "base work")
			require.NotEmpty(t, baseCP)
			env.GitPush("origin", "HEAD")
			env.RunPrePush("origin")
			require.True(t, env.CheckpointExistsOnRemote(bareDir, baseCP), "base checkpoint should be on remote")

			var localCP, remoteCP string
			switch state {
			case "ahead":
				// Local v1 gains a checkpoint that is never pushed; remote stays at base.
				localCP = createCheckpointedCommit(t, env, "local ahead", "ahead.go", "package ahead", "local ahead")
			case "behind":
				// Remote v1 advances independently; local stays at base with stale tracking.
				remoteCP = advanceRemoteV1(t, env, bareDir, "remote behind work")
			case "diverged":
				// Both sides add a checkpoint from the shared base.
				localCP = createCheckpointedCommit(t, env, "local diverge", "diverge.go", "package diverge", "local diverge")
				remoteCP = advanceRemoteV1(t, env, bareDir, "remote diverge work")
			case "disconnected":
				// Local v1 becomes an orphan sharing no ancestry with the remote base.
				localCP = "abcdef012345"
				forceLocalV1Orphan(t, env, localCP)
			}

			// A plain, pushable feature commit guarantees git invokes the pre-push hook
			// (an up-to-date push would skip it).
			trigger := "push-trigger-" + state + ".txt"
			env.WriteFile(trigger, "x")
			env.GitAdd(trigger)
			env.GitCommit("push trigger (" + state + ")")

			env.GitPushWithHooks("origin", "HEAD")

			// The base checkpoint survives on the remote in every state.
			require.True(t, env.CheckpointExistsOnRemote(bareDir, baseCP),
				"[%s] base checkpoint must always survive on the remote", state)

			switch state {
			case "ahead":
				require.True(t, env.CheckpointExistsOnRemote(bareDir, localCP),
					"ahead: local-only checkpoint should fast-forward onto the remote")
			case "behind":
				// Stale tracking makes the hook a safe no-op: the remote's own
				// checkpoint is untouched and nothing is lost. (Local reconcile to
				// a behind remote happens on READ triggers -- see the explain test.)
				require.True(t, env.CheckpointExistsOnRemote(bareDir, remoteCP),
					"behind: the remote's checkpoint must remain")
			case "diverged":
				require.True(t, env.CheckpointExistsOnRemote(bareDir, localCP),
					"diverged: local checkpoint must be replayed onto the remote")
				require.True(t, env.CheckpointExistsOnRemote(bareDir, remoteCP),
					"diverged: remote checkpoint must be preserved (not overwritten)")
				require.Equal(t, 1, env.GetBranchTipParentCount(paths.MetadataBranchName),
					"diverged: local v1 tip must be linear after replay (double-replay guard, #1260)")
			case "disconnected":
				require.True(t, env.CheckpointExistsOnRemote(bareDir, localCP),
					"disconnected: orphaned local checkpoint must be cherry-picked onto the remote")
			}

			// Double-replay guard (#1260): an immediately repeated pre-push must be
			// idempotent -- the remote checkpoint state stays byte-for-byte identical.
			before := env.RemoteCheckpointState(bareDir)
			env.RunPrePush("origin")
			after := env.RemoteCheckpointState(bareDir)
			require.Equal(t, before, after,
				"[%s] a repeated pre-push must not re-replay or otherwise mutate the remote", state)
		})
	}
}

// TestDivergedV1_ExplainFetchOnMiss covers the read-path (fetch-on-miss) reconcile
// for the {behind, diverged} states, extending the existing missing/ahead coverage
// (TestExplain_CheckpointFetchesFromRemoteWhenMissingLocally,
// TestExplain_CheckpointFetchDoesNotRewindLocalAheadBranch). Reading a remote-only
// checkpoint fetches and reconciles the local v1 (SafelyAdvanceLocalRef) so the
// remote checkpoint becomes visible while any local-only checkpoint survives.
func TestDivergedV1_ExplainFetchOnMiss(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"behind", "diverged"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()

			env := NewFeatureBranchEnv(t)
			bareDir := env.SetupBareRemote()

			baseCP := createCheckpointedCommit(t, env, "base work", "base.go", "package base", "base work")
			require.NotEmpty(t, baseCP)
			env.GitPush("origin", "HEAD")
			env.RunPrePush("origin")

			var localCP string
			if state == "diverged" {
				localCP = createCheckpointedCommit(t, env, "local diverge", "diverge.go", "package diverge", "local diverge")
			}

			remoteCP := advanceRemoteV1(t, env, bareDir, "remote read-path work")

			// The remote-only checkpoint is not present locally yet.
			require.False(t, env.FileExistsInBranch(paths.MetadataBranchName, CheckpointSummaryPath(remoteCP)),
				"[%s] remote checkpoint should be absent locally before the fetch-on-miss read", state)

			// Reading it triggers the fetch + reconcile.
			out := env.RunCLI("checkpoint", "explain", "--checkpoint", remoteCP)
			require.Contains(t, out, "remote read-path work",
				"[%s] explain should fetch the remote-only checkpoint and surface its prompt", state)

			// The local-only checkpoint (diverged case) must survive the reconcile.
			if localCP != "" {
				survived := env.RunCLI("checkpoint", "explain", "--checkpoint", localCP)
				require.Contains(t, survived, "local diverge",
					"diverged: local-only checkpoint must remain discoverable after fetch-on-miss reconcile")
			}
		})
	}
}

// TestDivergedV1_DoctorReconcilesDisconnected exercises the doctor trigger: a
// disconnected local v1 (orphan sharing no ancestry with the remote) is repaired
// by `entire doctor --force`, which cherry-picks the local checkpoints onto the
// remote tip so both sides survive. This is the CLI-level counterpart to the
// strategy unit tests for ReconcileDisconnectedMetadataRef.
func TestDivergedV1_DoctorReconcilesDisconnected(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	baseCP := createCheckpointedCommit(t, env, "base work", "base.go", "package base", "base work")
	require.NotEmpty(t, baseCP)
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")
	require.True(t, env.CheckpointExistsOnRemote(bareDir, baseCP))

	// Refresh the remote-tracking ref (doctor compares local v1 against
	// refs/remotes/origin/<v1> and does not fetch on its own), then orphan the
	// local v1 so it is disconnected from that tracked base.
	env.FetchMetadataBranch(bareDir)
	orphanCP := "abcdef012345"
	forceLocalV1Orphan(t, env, orphanCP)

	out := env.RunCLI("doctor", "--force")
	require.Contains(t, out, "reconciled", "doctor should report it reconciled the disconnected branches")

	// After the fix the local v1 carries BOTH the remote base checkpoint and the
	// orphaned local checkpoint on a single connected history.
	require.True(t, env.FileExistsInBranch(paths.MetadataBranchName, CheckpointSummaryPath(baseCP)),
		"doctor: remote base checkpoint must be present locally after reconcile")
	require.True(t, env.FileExistsInBranch(paths.MetadataBranchName, CheckpointSummaryPath(orphanCP)),
		"doctor: orphaned local checkpoint must survive the reconcile")
	require.Equal(t, 1, env.GetBranchTipParentCount(paths.MetadataBranchName),
		"doctor: reconciled v1 tip must be linear (cherry-pick, not merge)")
}
