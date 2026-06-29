package checkpointpolicy_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestSyncRemotePolicyDefaultsWhenRemoteMissing(t *testing.T) {
	localDir, repo, bareDir := initPolicyRemoteFixture(t)

	got, err := checkpointpolicy.Sync(t.Context(), repo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceDefaults, got.Source)
	require.Equal(t, checkpointpolicy.DefaultPolicy(), got.Policy)
	require.True(t, got.Hash.IsZero())
	require.True(t, got.RemoteHash.IsZero())
}

func TestSyncRemotePolicyFetchesAndPromotesMissingLocalRef(t *testing.T) {
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	remoteHash, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)
	got, err := checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceRemote, got.Source)
	require.Equal(t, remoteHash, got.Hash)
	require.Equal(t, remoteHash, got.RemoteHash)

	localState, err := checkpointpolicy.ReadLocal(t.Context(), localRepo)
	require.NoError(t, err)
	require.Equal(t, remoteHash, localState.Hash)
}

func TestSyncRemotePolicyDoesNotLeaveTempRefWhenSHAAlreadyMatches(t *testing.T) {
	localDir, repo, bareDir := initPolicyRemoteFixture(t)
	localHash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	pushPolicyRefWithGit(t, localDir, bareDir)

	got, err := checkpointpolicy.Sync(t.Context(), repo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceRemote, got.Source)
	require.Equal(t, localHash, got.Hash)
	require.Equal(t, localHash, got.RemoteHash)
	requireNoPolicyFetchRef(t, repo)
}

func TestSyncRemotePolicyKeepsDivergedLocalRef(t *testing.T) {
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	baseHash, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)
	_, err = checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	localHash, err := checkpointpolicy.WriteLocal(t.Context(), localRepo, baseHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)

	remoteHash, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, baseHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "refs-v1",
	})
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	got, err := checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceLocalDiverged, got.Source)
	require.Equal(t, localHash, got.Hash)
	require.Equal(t, remoteHash, got.RemoteHash)

	localState, err := checkpointpolicy.ReadLocal(t.Context(), localRepo)
	require.NoError(t, err)
	require.Equal(t, localHash, localState.Hash)
	requireNoPolicyFetchRef(t, localRepo)
}

func TestSyncRemotePolicyKeepsLocalRefAheadOfRemote(t *testing.T) {
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	baseHash, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)
	_, err = checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	localHash, err := checkpointpolicy.WriteLocal(t.Context(), localRepo, baseHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)

	got, err := checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceLocal, got.Source)
	require.Equal(t, localHash, got.Hash)
	require.Equal(t, baseHash, got.RemoteHash)

	localState, err := checkpointpolicy.ReadLocal(t.Context(), localRepo)
	require.NoError(t, err)
	require.Equal(t, localHash, localState.Hash)
	requireNoPolicyFetchRef(t, localRepo)
}

func TestSyncRemotePolicyRemovesTempRefWhenFetchedPolicyCannotBeRead(t *testing.T) {
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	writeRawPolicyCommit(t, remoteRepo, []byte(`{"checkpoint_version":`), plumbing.ZeroHash)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)
	_, err := checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.ErrorContains(t, err, "parse policy.json")
	requireNoPolicyFetchRef(t, localRepo)
}

func TestPushPolicyRejectsNonFastForward(t *testing.T) {
	firstDir, firstRepo, bareDir := initPolicyRemoteFixture(t)
	_, err := checkpointpolicy.WriteLocal(t.Context(), firstRepo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	pushPolicyRefWithGit(t, firstDir, bareDir)

	secondDir, secondRepo := initPolicyRepoWithDir(t)
	_, err = checkpointpolicy.WriteLocal(t.Context(), secondRepo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "refs-v1",
	})
	require.NoError(t, err)

	err = checkpointpolicy.Push(t.Context(), checkpointpolicy.Target{Remote: bareDir, Dir: secondDir})
	require.ErrorContains(t, err, "push checkpoint policy")
}

func TestResolveTargetUsesConfiguredCheckpointRemoteWithOriginOwnerMismatch(t *testing.T) {
	localDir, _ := initPolicyRepoWithDir(t)
	runPolicyGit(t, localDir, "remote", "add", "origin", "git@github.com:fork/cli.git")
	require.NoError(t, os.MkdirAll(filepath.Join(localDir, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, ".entire", "settings.json"), []byte(`{
  "enabled": true,
  "strategy_options": {
    "checkpoint_remote": {
      "provider": "github",
      "repo": "org/checkpoints"
    }
  }
}`), 0o600))

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	target, err := checkpointpolicy.ResolveTarget(t.Context())
	require.NoError(t, err)
	require.Equal(t, "git@github.com:org/checkpoints.git", target.Remote)
	wantDir, err := filepath.EvalSymlinks(localDir)
	require.NoError(t, err)
	gotDir, err := filepath.EvalSymlinks(target.Dir)
	require.NoError(t, err)
	require.Equal(t, wantDir, gotDir)
}

func TestResolveTargetUsesFetchURLPolicyTarget(t *testing.T) {
	localDir, _ := initPolicyRepoWithDir(t)
	upstreamDir := filepath.Join(t.TempDir(), "upstream.git")
	_, err := git.PlainInit(upstreamDir, true)
	require.NoError(t, err)
	runPolicyGit(t, localDir, "remote", "add", "upstream", upstreamDir)

	t.Chdir(localDir)
	paths.ClearWorktreeRootCache()

	_, err = checkpointpolicy.ResolveTarget(t.Context())
	require.ErrorContains(t, err, "no fetch URL found")
}

func initPolicyRemoteFixture(t *testing.T) (string, *git.Repository, string) {
	t.Helper()
	localDir, repo := initPolicyRepoWithDir(t)
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err := git.PlainInit(bareDir, true)
	require.NoError(t, err)
	return localDir, repo, bareDir
}

func initPolicyRepoWithDir(t *testing.T) (string, *git.Repository) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return dir, repo
}

func pushPolicyRefWithGit(t *testing.T, dir, remote string) {
	t.Helper()
	refspec := checkpointpolicy.RefName.String() + ":" + checkpointpolicy.RefName.String()
	runPolicyGit(t, dir, "push", remote, refspec)
}

func runPolicyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func requireNoPolicyFetchRef(t *testing.T, repo *git.Repository) {
	t.Helper()
	_, err := repo.Reference(plumbing.ReferenceName("refs/entire/policies/checkpoint-fetch"), true)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}
