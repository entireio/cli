package remote

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestIsShallowRepository_Layouts(t *testing.T) {
	gitenv.IsolateRepository(t)
	origin, clone := setupShallowClone(t)
	full := t.TempDir()
	testutil.InitRepo(t, full)
	t.Chdir(full)
	require.False(t, isShallowRepository(t.Context(), ""), "empty Dir uses CWD")
	require.True(t, isShallowRepository(t.Context(), clone), "explicit Dir wins over CWD")

	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, clone, "worktree", "add", "--detach", linked)
	require.True(t, isShallowRepository(t.Context(), linked), "linked worktree shares shallow boundaries")
	t.Chdir(linked)
	require.True(t, isShallowRepository(t.Context(), ""))
	require.False(t, isShallowRepository(t.Context(), full))

	bare := filepath.Join(t.TempDir(), "bare.git")
	testutil.RunGit(t, full, "clone", "--bare", "--depth=1", "--branch", "main", "file://"+origin, bare)
	require.True(t, isShallowRepository(t.Context(), bare))
	testutil.RunGit(t, clone, "fetch", "--unshallow", "origin")
	require.False(t, isShallowRepository(t.Context(), clone))
	require.False(t, isShallowRepository(t.Context(), linked), "must observe removed common shallow state")
}

func TestIsShallowRepository_FailureAndFileSemantics(t *testing.T) {
	for _, state := range []string{"not a repository", "canceled", "empty file", "malformed file"} {
		t.Run(state, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			dir := t.TempDir()
			t.Chdir(dir)
			if state != "not a repository" {
				testutil.InitRepo(t, dir)
			}
			ctx := t.Context()
			switch state {
			case "empty file", "malformed file":
				content := ""
				if state == "malformed file" {
					content = "not an object id\n"
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(content), 0o600))
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			cmd := execx.NonInteractive(ctx, "git", "rev-parse", "--is-shallow-repository")
			cmd.Dir = dir
			cmd.Env = testutil.GitIsolatedEnv()
			out, err := cmd.Output()
			want := err == nil && strings.TrimSpace(string(out)) == "true"
			require.Equal(t, want, isShallowRepository(ctx, dir))
			// The existing best-effort bool API deliberately swallows failed reads.
			if state == "not a repository" || state == "canceled" || state == "malformed file" {
				require.False(t, want)
			} else {
				require.True(t, want, "native Git treats an existing empty shallow file as shallow")
			}
		})
	}
}
