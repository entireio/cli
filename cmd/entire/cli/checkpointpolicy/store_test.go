package checkpointpolicy_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

func TestReadLocalPolicyDefaultsWhenRefMissing(t *testing.T) {
	t.Parallel()
	repo := initPolicyRepo(t)
	got, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceDefaults, got.Source)
	require.Equal(t, checkpointpolicy.DefaultPolicy(), got.Policy)
	require.True(t, got.Hash.IsZero())
}

func TestWriteAndReadLocalPolicy(t *testing.T) {
	t.Parallel()
	repo := initPolicyRepo(t)
	policy := checkpointpolicy.Policy{
		CheckpointVersion:    checkpoint.CheckpointVersionBranchV1,
		CheckpointMinVersion: checkpoint.CheckpointVersionBranchV1,
	}
	hash, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, policy)
	require.NoError(t, err)
	require.False(t, hash.IsZero())

	got, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceLocal, got.Source)
	require.Equal(t, hash, got.Hash)
	require.Equal(t, policy, got.Policy)
}

func TestReadLocalPolicyRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	repo := initPolicyRepo(t)
	writeRawPolicyCommit(t, repo, []byte(`{"checkpoint_version":`), plumbing.ZeroHash)

	_, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.ErrorContains(t, err, "parse policy.json")
}

func TestReadLocalPolicyRejectsOversizedJSON(t *testing.T) {
	t.Parallel()
	repo := initPolicyRepo(t)
	data := []byte(`{"checkpoint_version":"branch-v1","checkpoint_min_version":"` + strings.Repeat("a", 70*1024) + `"}`)
	writeRawPolicyCommit(t, repo, data, plumbing.ZeroHash)

	_, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.ErrorContains(t, err, "parse policy.json")
}

func TestReadLocalPolicyAllowsUnsupportedPolicy(t *testing.T) {
	t.Parallel()
	repo := initPolicyRepo(t)
	policy := checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v1",
		CheckpointMinVersion: "refs-v1",
	}
	data, err := json.Marshal(policy)
	require.NoError(t, err)
	hash := writeRawPolicyCommit(t, repo, data, plumbing.ZeroHash)

	got, err := checkpointpolicy.ReadLocal(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceLocal, got.Source)
	require.Equal(t, hash, got.Hash)
	require.Equal(t, policy, got.Policy)
}

func initPolicyRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return repo
}

func writeRawPolicyCommit(t *testing.T, repo *git.Repository, data []byte, parent plumbing.Hash) plumbing.Hash {
	t.Helper()
	blobHash, err := checkpoint.CreateBlobFromContent(repo, data)
	require.NoError(t, err)
	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		checkpointpolicy.PolicyFileName: {Name: checkpointpolicy.PolicyFileName, Mode: filemode.Regular, Hash: blobHash},
	})
	require.NoError(t, err)
	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, parent, "raw policy", "Test", "test@example.com")
	require.NoError(t, err)
	require.NoError(t, checkpointpolicy.SetRef(repo, checkpointpolicy.RefName, commitHash))
	return commitHash
}
