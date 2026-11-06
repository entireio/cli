package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// newDirtyTrackedRepo returns a repository with a.txt, b.txt and c.txt
// committed, then a.txt modified, b.txt deleted and u.txt untracked, and
// changes into it.
func newDirtyTrackedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	testutil.InitRepo(t, dir)
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		testutil.WriteFile(t, dir, f, f+"\n")
	}
	testutil.GitAdd(t, dir, "a.txt", "b.txt", "c.txt")
	testutil.GitCommit(t, dir, "Add files")
	testutil.WriteFile(t, dir, "a.txt", "a, edited\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "b.txt")))
	testutil.WriteFile(t, dir, "u.txt", "untracked\n")
	return dir
}

func TestStatusFilesForState_ListsDirtyTrackedFiles(t *testing.T) {
	// Not parallel: newDirtyTrackedRepo uses t.Chdir.
	newDirtyTrackedRepo(t)

	untracked, dirty, err := statusFilesForState(context.Background())
	require.NoError(t, err)
	slices.Sort(dirty)
	require.Equal(t, []string{"u.txt"}, untracked)
	require.Equal(t, []string{"a.txt", "b.txt"}, dirty)
}

// TestStatusClaim_LeavesOutUnchangedDirtyFiles pins direction 1 of #3011: a
// tracked file already dirty at prompt start is claimed only once its stat
// changes.
func TestStatusClaim_LeavesOutUnchangedDirtyFiles(t *testing.T) {
	// Not parallel: newDirtyTrackedRepo uses t.Chdir.
	dir := newDirtyTrackedRepo(t)
	ctx := context.Background()
	claim := statusClaim{repoRoot: dir, dirtyTracked: statDirtyTrackedFiles(ctx, []string{"a.txt", "b.txt"})}
	require.Equal(t, UntrackedFileStat{}, claim.dirtyTracked["b.txt"], "a deleted file gets the zero stat")

	modified, created, deleted := claim.narrow(ctx, []string{"a.txt"}, []string{"u.txt"}, []string{"b.txt"})
	require.Empty(t, modified, "a.txt was dirty before and is unchanged")
	require.Empty(t, deleted, "b.txt was deleted before and is still deleted")
	require.Equal(t, []string{"u.txt"}, created, "new files are not judged by the tracked baseline")

	testutil.WriteFile(t, dir, "a.txt", "a, edited again\n")
	testutil.WriteFile(t, dir, "b.txt", "b, restored with other content\n")
	testutil.WriteFile(t, dir, "c.txt", "c, newly dirty\n")
	modified, _, _ = claim.narrow(ctx, []string{"a.txt", "b.txt", "c.txt"}, nil, nil)
	require.Equal(t, []string{"a.txt", "b.txt", "c.txt"}, modified)
}

// TestStatusClaim_NoBaselineNarrowsNothing pins the fallback for state
// written before the baseline existed, or with no state at all: every file git
// status lists is kept, as before.
func TestStatusClaim_NoBaselineNarrowsNothing(t *testing.T) {
	t.Parallel()
	var noState *PrePromptState
	oldState := &PrePromptState{Timestamp: "not a time"}
	for _, claim := range []statusClaim{noState.statusClaim("", "s", nil), oldState.statusClaim("", "s", nil)} {
		modified, created, deleted := claim.narrow(context.Background(), []string{"a.txt"}, []string{"n.txt"}, []string{"d.txt"})
		require.Equal(t, []string{"a.txt"}, modified)
		require.Equal(t, []string{"n.txt"}, created)
		require.Equal(t, []string{"d.txt"}, deleted)
	}
}

func TestParseCaptureTime(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	require.True(t, parseCaptureTime(now.Format(time.RFC3339Nano)).Equal(now))
	require.True(t, parseCaptureTime(now.Format(time.RFC3339)).Equal(now.Truncate(time.Second)), "state written before nanoseconds were recorded")
	require.True(t, parseCaptureTime("").IsZero())
}
