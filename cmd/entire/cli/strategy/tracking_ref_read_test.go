package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestRemoteHasTrackingRefs_PrefixAndStorage(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	t.Chdir(root)
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin-other/main", head)
	require.False(t, remoteHasTrackingRefs(t.Context(), "origin"), "do not match another remote's prefix")
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin-other"))
	require.False(t, remoteHasTrackingRefs(t.Context(), ""))
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/topic/nested", head)
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	testutil.RunGit(t, root, "pack-refs", "--all")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, root, "worktree", "add", "--detach", linked)
	t.Chdir(linked)
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	testutil.RunGit(t, root, "update-ref", "-d", "refs/remotes/origin/topic/nested")
	require.False(t, remoteHasTrackingRefs(t.Context(), "origin"), "must observe native deletion of a packed ref")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.False(t, remoteHasTrackingRefs(ctx, "origin-other"))
}

// Loose refs iterate before packed ones. An unusable loose ref must not hide a
// valid packed ref: native for-each-ref sorts by name and reports the packed
// refs/remotes/origin/main ahead of the broken refs/remotes/origin/zz.
func TestRemoteHasTrackingRefs_SkipsUnusableRefs(t *testing.T) {
	gitenv.IsolateRepository(t)
	root, _, head := initCountTestRepo(t)
	t.Chdir(root)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	testutil.RunGit(t, root, "update-ref", "refs/remotes/origin/main", head)
	testutil.RunGit(t, root, "pack-refs", "--all")
	looseDir := filepath.Join(root, ".git", "refs", "remotes", "origin")
	require.NoError(t, os.MkdirAll(looseDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(looseDir, "zz"), []byte(strings.Repeat("1", 40)+"\n"), 0o600))
	testutil.RunGit(t, root, "symbolic-ref", "refs/remotes/origin/dangling", "refs/remotes/origin/absent")
	require.Equal(t, remoteHasTrackingRefsNative(t.Context(), "origin"), remoteHasTrackingRefs(t.Context(), "origin"))
	require.True(t, remoteHasTrackingRefs(t.Context(), "origin"))
	testutil.RunGit(t, root, "update-ref", "-d", "refs/remotes/origin/main")
	require.False(t, remoteHasTrackingRefs(t.Context(), "origin"), "only unusable refs remain")
}
