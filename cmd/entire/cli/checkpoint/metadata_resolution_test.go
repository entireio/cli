package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestCheckpointMetadataResolutionPreservesCallerPoliciesAndObservesRepair(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	testutil.InitRepo(t, repoRoot)
	repo, err := gitrepo.OpenPath(repoRoot)
	require.NoError(t, err)
	defer repo.Close()

	commonFile := filepath.Join(repoRoot, ".git", "commondir")
	require.NoError(t, os.WriteFile(commonFile, []byte("missing\n"), 0o600))

	queue, err := PushQueueForRepo(context.Background(), repo)
	require.ErrorContains(t, err, "resolve git common dir for push queue")
	require.Nil(t, queue, "required queue metadata must fail closed")
	require.Nil(t, repoRedactCache(context.Background(), repo), "optional redaction cache must fail open")

	require.NoError(t, os.Remove(commonFile))
	queue, err = PushQueueForRepo(context.Background(), repo)
	require.NoError(t, err, "a metadata repair must be observed without clearing a cache")
	require.NotNil(t, queue)
}
