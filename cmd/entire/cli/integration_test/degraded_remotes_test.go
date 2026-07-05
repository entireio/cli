//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// F-group: degraded remotes & protocol edges.

// installProtectV1BranchHook writes a pre-receive hook on the bare remote that
// rejects any update to refs/heads/entire/checkpoints/v1 with a GH013-style
// message (the shape classifyPushOutput recognizes as a protected-ref
// rejection). Non-checkpoint refs (the user's branch) are accepted, so the
// hook models a repository ruleset that protects the entire/* namespace.
func installProtectV1BranchHook(t *testing.T, bareDir string) {
	t.Helper()
	hooksDir := filepath.Join(bareDir, "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))
	script := "#!/bin/sh\n" +
		"while read -r _old _new ref; do\n" +
		"  case \"$ref\" in\n" +
		"    refs/heads/entire/checkpoints/v1)\n" +
		"      echo 'remote: error: GH013: Repository rule violations found for refs/heads/entire/checkpoints/v1' >&2\n" +
		"      echo 'remote: Cannot update this protected ref.' >&2\n" +
		"      exit 1;;\n" +
		"  esac\n" +
		"done\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "pre-receive"), []byte(script), 0o755)) //nolint:gosec // hook must be executable
}

// TestProtectedV1Branch_UserPushUnaffected is F1 (regression #1033): when the
// remote rejects the checkpoint v1 branch with a GH013-style protected-ref
// error, the pre-push hook prints a loud one-shot banner and returns without
// retrying — the user's own push still succeeds. git-branch only (the v1
// branch is git-branch's checkpoint ref).
func TestProtectedV1Branch_UserPushUnaffected(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()
	installProtectV1BranchHook(t, bareDir)

	_ = createCheckpointedCommit(t, env, "Add auth", "auth.go", "package auth", "Add auth")

	// A fresh pushable commit so git actually invokes the pre-push hook and has
	// something to transfer on the user branch.
	env.WriteFile("feature.txt", "feature")
	env.GitAdd("feature.txt")
	env.GitCommit("feature commit")

	out, err := env.GitPushArgsWithHooks("origin", "HEAD")
	require.NoError(t, err, "user push must succeed even when the checkpoint v1 branch is protected:\n%s", out)

	// The loud protected-ref banner is surfaced (not a silent failure).
	require.Contains(t, out, "BLOCKED: remote rejected push",
		"a protected-ref rejection should print the loud banner:\n%s", out)

	// The checkpoint branch was rejected — it must not be on the remote.
	require.False(t, env.CheckpointsPresentOnRemote(bareDir),
		"the protected v1 branch must not land on the remote")

	// The user branch advanced to the new commit (their push was unaffected).
	localHead := localGitRevParse(t, env, "HEAD")
	require.Equal(t, localHead, remoteBranchTip(t, bareDir, "feature/test-branch"),
		"the user's own push should land the feature commit despite the checkpoint rejection")

	// No retry loop: a second push with nothing new for the user still succeeds
	// fast and does not smuggle the checkpoint branch onto the remote.
	out2, err2 := env.GitPushArgsWithHooks("origin", "HEAD")
	require.NoError(t, err2, "a repeat push must still succeed:\n%s", out2)
	require.False(t, env.CheckpointsPresentOnRemote(bareDir),
		"the protected v1 branch must still be absent after a repeat push")
}

// localGitRevParse resolves a revision to its commit hash in the working repo.
func localGitRevParse(t *testing.T, env *TestEnv, rev string) string {
	t.Helper()
	cmd := exec.CommandContext(env.T.Context(), "git", "rev-parse", rev)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse %s", rev)
	return string(out[:len(out)-1]) // strip trailing newline
}

// seedBlockedCheckpointPolicy writes a local checkpoint policy whose
// checkpoint_min_version this CLI cannot read (branch-v2), so
// CanSatisfyPolicy is false. Because the bare remote carries no policy ref,
// syncCheckpointPolicyForPrePush leaves this local policy in place, which makes
// checkpointPolicyAllowsGitHook return false at pre-push time.
func seedBlockedCheckpointPolicy(t *testing.T, env *TestEnv) {
	t.Helper()
	repo, err := gitrepo.OpenPath(env.RepoDir)
	require.NoError(t, err)
	_, err = checkpointpolicy.WriteLocal(env.T.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "branch-v2",
		CheckpointMinVersion: "branch-v2",
	})
	require.NoError(t, err, "seed blocked local checkpoint policy")
}

// TestGitRefsBlockedPolicy_SkipsRefPushNotUserPush is F4 (regression 7bbdad09c
// follow-up): the git-refs pre-push path honors the checkpoint policy. A local
// policy this CLI cannot satisfy skips the per-checkpoint ref push (leaving the
// refs queued for a later, upgraded run) but does NOT abort the user's push.
func TestGitRefsBlockedPolicy_SkipsRefPushNotUserPush(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs
	bareDir := env.SetupBareRemote()

	checkpointID := createCheckpointedCommit(t, env, "Add service", "svc.go", "package svc", "Add service")
	require.NotEmpty(t, checkpointID)
	ref := checkpointRefName(checkpointID)
	require.Contains(t, env.PushQueueRefs(), ref, "the new checkpoint ref should be queued before push")

	// Block the policy right before the push.
	seedBlockedCheckpointPolicy(t, env)

	out, err := env.GitPushArgsWithHooks("origin", "HEAD")
	require.NoError(t, err, "a blocked checkpoint policy must not abort the user push:\n%s", out)

	// User branch landed; checkpoint ref did not; queue still holds the ref.
	require.True(t, env.BranchExistsOnRemote(bareDir, "feature/test-branch"),
		"the user branch should land even though the policy blocked checkpoint sync")
	require.False(t, env.CheckpointExistsOnRemote(bareDir, checkpointID),
		"a blocked policy must skip pushing the checkpoint ref")
	require.Contains(t, env.PushQueueRefs(), ref,
		"a blocked policy must leave the checkpoint ref queued for a later retry")
}

// detachHead detaches HEAD at its current commit (a git-plumbing operation the
// TestEnv has no direct helper for). Subsequent commits advance the detached
// HEAD without moving any branch.
func detachHead(t *testing.T, env *TestEnv) {
	t.Helper()
	cmd := exec.CommandContext(env.T.Context(), "git", "checkout", "--detach", "HEAD")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout --detach failed: %v\n%s", err, output)
	}
}

// TestDetachedHead_CheckpointPushAndResume is F5: a session that runs and
// checkpoints while HEAD is detached, pushed with `git push origin HEAD:<branch>`
// via the real hook, is discoverable and readable after a fresh clone. This
// pins the current behavior. git-branch (default) backend.
func TestDetachedHead_CheckpointPushAndResume(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	bareDir := env.SetupBareRemote()

	detachHead(t, env)

	checkpointID := createCheckpointedCommit(t, env, "Detached work", "detached.go", "package detached", "Detached work")
	require.NotEmpty(t, checkpointID, "a checkpoint should be created while HEAD is detached")
	require.True(t, env.CheckpointsPresentLocally(), "checkpoint should exist locally after a detached-HEAD session")

	// Push the detached work to a named branch on the remote through the real
	// pre-push hook (git feeds the hook a "HEAD <sha> refs/heads/detached-work <sha>"
	// stdin line).
	out, err := env.GitPushArgsWithHooks("origin", "HEAD:refs/heads/detached-work")
	require.NoError(t, err, "pushing detached HEAD to a branch via the real hook should succeed:\n%s", out)
	require.True(t, env.CheckpointExistsOnRemote(bareDir, checkpointID),
		"the detached-HEAD checkpoint should sync to the remote")

	// A fresh clone can read the checkpoint (auto-fetch on read).
	clone := env.CloneFrom(bareDir)
	explain := clone.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
	require.Contains(t, explain, "Detached work",
		"the detached-HEAD checkpoint should be readable from a fresh clone")
}
