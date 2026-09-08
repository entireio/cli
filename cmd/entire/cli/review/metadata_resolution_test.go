package review

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestReviewManifestMetadataFailureAndRepair(t *testing.T) {
	main := t.TempDir()
	testutil.InitRepo(t, main)
	testutil.WriteFile(t, main, "initial.txt", "initial\n")
	testutil.GitAdd(t, main, "initial.txt")
	testutil.GitCommit(t, main, "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "linked", linked)
	t.Chdir(linked)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	metadata, err := gitrepo.ResolveWorktreeMetadata(linked)
	require.NoError(t, err)
	manifest := LocalReviewManifest{WorktreePath: linked, CreatedAt: time.Now().UTC(), Sources: []ManifestSource{{SessionID: "metadata-review", Output: "preserved"}}}
	require.NoError(t, writeLocalReviewManifest(t.Context(), manifest))
	require.FileExists(t, filepath.Join(main, ".git", localReviewManifestName, localReviewManifestFilename(manifest)))
	pointer := filepath.Join(metadata.GitDir, "commondir")
	original, err := os.ReadFile(pointer)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pointer, []byte("missing\n"), 0o600))
	manifest.Sources[0].Output = "must not overwrite"
	require.Error(t, writeLocalReviewManifest(t.Context(), manifest))
	_, err = loadLocalReviewManifests(t.Context(), linked)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(pointer, original, 0o600))
	loaded, err := loadLocalReviewManifests(t.Context(), linked)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	require.Equal(t, "preserved", loaded[0].Sources[0].Output)
}
