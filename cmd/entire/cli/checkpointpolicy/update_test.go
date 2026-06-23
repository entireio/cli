package checkpointpolicy_test

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestUpdateRejectsDowngradeFromRemoteWithoutForce(t *testing.T) {
	t.Parallel()
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	_, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "refs-v1",
	})
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)

	_, err = checkpointpolicy.Update(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir}, checkpointpolicy.UpdateOptions{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	})
	require.ErrorContains(t, err, "would downgrade checkpoint_version")

	localState, err := checkpointpolicy.ReadLocal(t.Context(), localRepo)
	require.NoError(t, err)
	require.True(t, localState.Hash.IsZero())
}

func TestUpdateAllowsDowngradeWithForce(t *testing.T) {
	t.Parallel()
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	remoteHash, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "refs-v1",
	})
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)
	got, err := checkpointpolicy.Update(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir}, checkpointpolicy.UpdateOptions{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
		Force:                true,
	})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceLocal, got.Source)
	require.Equal(t, checkpointpolicy.Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}, got.Policy)

	commit, err := localRepo.CommitObject(got.Hash)
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{remoteHash}, commit.ParentHashes)
}

func TestUpdatePreservesLocalPolicyAheadOfRemote(t *testing.T) {
	t.Parallel()
	remoteDir, remoteRepo, bareDir := initPolicyRemoteFixture(t)
	baseHash, err := checkpointpolicy.WriteLocal(t.Context(), remoteRepo, plumbing.ZeroHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)
	pushPolicyRefWithGit(t, remoteDir, bareDir)

	localDir, localRepo := initPolicyRepoWithDir(t)
	_, err = checkpointpolicy.Sync(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir})
	require.NoError(t, err)
	localHash, err := checkpointpolicy.WriteLocal(t.Context(), localRepo, baseHash, checkpointpolicy.DefaultPolicy())
	require.NoError(t, err)

	got, err := checkpointpolicy.Update(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir}, checkpointpolicy.UpdateOptions{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	})
	require.NoError(t, err)
	require.Equal(t, baseHash, got.RemoteHash)

	commit, err := localRepo.CommitObject(got.Hash)
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{localHash}, commit.ParentHashes)
}

func TestUpdateRejectsDivergedLocalPolicy(t *testing.T) {
	t.Parallel()
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

	_, err = checkpointpolicy.Update(t.Context(), localRepo, checkpointpolicy.Target{Remote: bareDir, Dir: localDir}, checkpointpolicy.UpdateOptions{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	})
	require.ErrorContains(t, err, "local checkpoint policy")
	require.ErrorContains(t, err, "diverges from remote")

	localState, err := checkpointpolicy.ReadLocal(t.Context(), localRepo)
	require.NoError(t, err)
	require.Equal(t, localHash, localState.Hash)
	require.NotEqual(t, remoteHash, localState.Hash)
}
