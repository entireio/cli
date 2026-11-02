package strategy

import (
	"context"
	"path/filepath"
	"testing"

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
