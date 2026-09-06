package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestTrackedAttributionBaseline(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	testutil.InitRepo(t, dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	ctx := context.Background()
	for _, name := range []string{"dirty", "deleted", "clean", "new-delete"} {
		require.NoError(t, os.WriteFile(name, []byte("original\n"), 0o644))
	}
	testutil.GitAdd(t, dir, ".")
	testutil.GitCommit(t, dir, "baseline")
	require.NoError(t, os.WriteFile("dirty", []byte("prior edit\n"), 0o644))
	require.NoError(t, os.Remove("deleted"))
	require.NoError(t, os.WriteFile("staged", []byte("prior staged addition\n"), 0o644))
	testutil.GitAdd(t, dir, "staged")
	require.NoError(t, CapturePrePromptState(ctx, claudecode.NewClaudeCodeAgent(), "attribution-test", ""))
	before, err := LoadPrePromptState(ctx, "attribution-test")
	require.NoError(t, err)
	require.NotNil(t, before.TrackedBaseline)
	changes, err := DetectFileChanges(ctx, before.PreUntrackedFiles())
	require.NoError(t, err)
	excludeUnchangedTracked(ctx, changes, before.TrackedBaseline)
	require.Empty(t, changes.Modified, "read-only turn must not claim prior edits or staged additions")
	require.Empty(t, changes.Deleted, "read-only turn must not claim prior deletions")
	require.NoError(t, os.WriteFile("dirty", []byte("this turn edits an already dirty file\n"), 0o644))
	require.NoError(t, os.WriteFile("clean", []byte("this turn edits a clean file\n"), 0o644))
	require.NoError(t, os.Remove("new-delete"))
	changes, err = DetectFileChanges(ctx, before.PreUntrackedFiles())
	require.NoError(t, err)
	excludeUnchangedTracked(ctx, changes, before.TrackedBaseline)
	require.ElementsMatch(t, []string{"dirty", "clean"}, changes.Modified)
	require.Equal(t, []string{"new-delete"}, changes.Deleted)
	require.NoError(t, CapturePreTaskState(ctx, "task-attribution"))
	task, err := LoadPreTaskState(ctx, "task-attribution")
	require.NoError(t, err)
	require.Equal(t, captureTrackedBaseline(ctx), task.TrackedBaseline)
	require.FileExists(t, filepath.Join(dir, "dirty"))
}

func TestTrackedAttributionMissingBaseline(t *testing.T) {
	changes := &FileChanges{Modified: []string{"unproven"}, Deleted: []string{"unproven"}, New: []string{"new"}}
	excludeUnchangedTracked(context.Background(), changes, nil)
	require.Empty(t, changes.Modified)
	require.Empty(t, changes.Deleted)
	require.Equal(t, []string{"new"}, changes.New)
}
