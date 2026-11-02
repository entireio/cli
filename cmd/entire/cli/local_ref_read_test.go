package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestMetadataTrackingRefExists_CommitRequired(t *testing.T) {
	gitenv.IsolateRepository(t)
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	testutil.WriteFile(t, root, "file", "content\n")
	testutil.RunGit(t, root, "add", ".")
	testutil.RunGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
	head := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD"))
	blob := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD:file"))
	ref := "refs/remotes/origin/" + paths.MetadataBranchName
	require.False(t, metadataTrackingRefExists(t.Context(), "origin"))
	testutil.RunGit(t, root, "update-ref", ref, blob)
	require.False(t, metadataTrackingRefExists(t.Context(), "origin"), "a non-commit ref is not a usable tracking tip")
	testutil.RunGit(t, root, "update-ref", ref, head)
	require.True(t, metadataTrackingRefExists(t.Context(), "origin"))
	testutil.RunGit(t, root, "pack-refs", "--all")
	require.True(t, metadataTrackingRefExists(t.Context(), "origin"))
	require.False(t, metadataTrackingRefExists(t.Context(), "orig"), "remote names are exact, not prefixes")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.False(t, metadataTrackingRefExists(ctx, "origin"))
}

func TestResolveWorktreeBranchGit_HeadStates(t *testing.T) {
	gitenv.IsolateRepository(t)
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	testutil.RunGit(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
	require.Equal(t, detachedHEADDisplay, resolveWorktreeBranchGit(t.Context(), root), "native rev-parse fails for unborn HEAD")
	testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
	require.Equal(t, "main", resolveWorktreeBranchGit(t.Context(), root))
	testutil.RunGit(t, root, "pack-refs", "--all")
	require.Equal(t, "main", resolveWorktreeBranchGit(t.Context(), root))
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, root, "worktree", "add", "-b", "feature/nested", linked)
	require.Equal(t, "feature/nested", resolveWorktreeBranchGit(t.Context(), linked))
	testutil.RunGit(t, linked, "checkout", "--detach")
	require.Equal(t, detachedHEADDisplay, resolveWorktreeBranchGit(t.Context(), linked))
	require.Equal(t, detachedHEADDisplay, resolveWorktreeBranchGit(t.Context(), t.TempDir()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, detachedHEADDisplay, resolveWorktreeBranchGit(ctx, root))
}
