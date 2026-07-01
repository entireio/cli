package cli

import (
	"context"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

const (
	unsupportedCheckpointMetadataVersion = "2.0.0"
	unsupportedCheckpointPolicyExpr      = ">=2.0.0"
)

func writeMalformedCheckpointPolicyForCLITest(t *testing.T, repo *git.Repository) {
	t.Helper()
	blobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(`{"checkpoint_version":`))
	require.NoError(t, err)
	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		checkpointpolicy.PolicyFileName: {Name: checkpointpolicy.PolicyFileName, Mode: filemode.Regular, Hash: blobHash},
	})
	require.NoError(t, err)
	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, treeHash, plumbing.ZeroHash, "malformed checkpoint policy", "Test", "test@example.com")
	require.NoError(t, err)
	require.NoError(t, checkpointpolicy.SetRef(repo, checkpointpolicy.RefName, commitHash))
}

func writeUnsupportedCheckpointPolicyForCLITest(t *testing.T, repo *git.Repository) {
	t.Helper()
	_, err := checkpointpolicy.WriteLocal(t.Context(), repo, plumbing.ZeroHash, checkpointpolicy.Policy{
		CheckpointVersion: unsupportedCheckpointPolicyExpr,
	})
	require.NoError(t, err)
}
