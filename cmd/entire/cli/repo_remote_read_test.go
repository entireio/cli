package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestListGitRemotes_ConfigScopes(t *testing.T) {
	gitenv.IsolateRepository(t)
	root := t.TempDir()
	testutil.InitRepo(t, root)
	t.Chdir(root)
	testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
	testutil.RunGit(t, root, "remote", "add", "local", "https://example.invalid/local")
	included := filepath.Join(t.TempDir(), "included.config")
	testutil.RunGit(t, root, "config", "--file", included, "remote.included.url", "https://example.invalid/included")
	testutil.RunGit(t, root, "config", "include.path", included)
	// Multiple URLs and config sources must not duplicate names.
	testutil.RunGit(t, root, "config", "--file", included, "remote.local.pushurl", "https://example.invalid/push")
	testutil.RunGit(t, root, "config", "extensions.worktreeConfig", "true")
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, root, "worktree", "add", "--detach", linked)
	testutil.RunGit(t, linked, "config", "--worktree", "remote.worktree.url", "https://example.invalid/worktree")

	remotes, err := listGitRemotes(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"local": true, "included": true}, remotes)
	remotes, err = listGitRemotes(t.Context(), linked)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"local": true, "included": true, "worktree": true}, remotes)
}

func TestListGitRemotes_ReadFailures(t *testing.T) {
	for _, state := range []string{"not a repository", "malformed config", "canceled"} {
		t.Run(state, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			root := t.TempDir()
			t.Chdir(root)
			if state != "not a repository" {
				testutil.InitRepo(t, root)
			}
			ctx := t.Context()
			switch state {
			case "malformed config":
				require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[broken\n"), 0o600))
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			remotes, err := listGitRemotes(ctx, root)
			require.Error(t, err, "read failures must not look like an empty remote list")
			require.Nil(t, remotes)
		})
	}
}
