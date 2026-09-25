package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestHeadCheckpointFlags_RepositoryLayouts(t *testing.T) {
	for _, layout := range []string{"detached", "linked", "subdirectory"} {
		t.Run(layout, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			repo := setupHeadFlagsRepo(t)
			defer repo.Close()
			t.Cleanup(paths.ClearWorktreeRootCache)
			cpID := writeHeadCheckpointWithFlags(t, repo, true, false)
			root, err := os.Getwd()
			require.NoError(t, err)
			switch layout {
			case "detached":
				testutil.RunGit(t, root, "checkout", "--detach")
			case "linked":
				linked := filepath.Join(t.TempDir(), "linked")
				testutil.RunGit(t, root, "worktree", "add", "--detach", linked)
				// Main worktree no longer has a trailer. Reading common HEAD instead of
				// linked HEAD must not accidentally make this test pass.
				testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", "main without checkpoint")
				t.Chdir(linked)
			case "subdirectory":
				nested := filepath.Join(root, "nested")
				require.NoError(t, os.MkdirAll(nested, 0o755))
				t.Chdir(nested)
			}
			paths.ClearWorktreeRootCache()
			review, investigation, info := headCheckpointFlags(t.Context())
			require.True(t, review)
			require.False(t, investigation)
			require.Contains(t, info, cpID.String())
		})
	}
}

func TestHeadCheckpointFlags_ReadFailures(t *testing.T) {
	for _, state := range []string{"not a repository", "unborn", "missing metadata", "malformed trailer", "canceled"} {
		t.Run(state, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			root := t.TempDir()
			t.Chdir(root)
			paths.ClearWorktreeRootCache()
			t.Cleanup(paths.ClearWorktreeRootCache)
			if state != "not a repository" {
				testutil.InitRepo(t, root)
			}
			if state == "missing metadata" || state == "malformed trailer" || state == "canceled" {
				message := "initial\n\nEntire-Checkpoint: aabbccdd1122"
				if state == "malformed trailer" {
					message = "initial\n\nEntire-Checkpoint: invalid"
				}
				testutil.RunGit(t, root, "commit", "--allow-empty", "--no-gpg-sign", "-m", message)
			}
			ctx := t.Context()
			if state == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			review, investigation, info := headCheckpointFlags(ctx)
			require.False(t, review)
			require.False(t, investigation)
			require.Empty(t, info)
		})
	}
}
