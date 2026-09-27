package gitexec_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

// Production HeadSHA inherits its environment; isolate it as well as fixture
// commands. These tests intentionally change process state and run serially.
func isolateHeadRead(t *testing.T) {
	t.Helper()
	gitenv.IsolateRepository(t)
	t.Chdir(t.TempDir())
}

func TestHeadSHA_RepositoryLayouts(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, refs := range []string{"files", "reftable"} {
			t.Run(format+"/"+refs, func(t *testing.T) {
				isolateHeadRead(t)
				dir := t.TempDir()
				// Native init is needed to select the object and reference formats.
				testutil.RunGit(t, dir, "init", "--object-format="+format, "--ref-format="+refs, "-b", "main")
				testutil.RunGit(t, dir, "config", "user.name", "Test")
				testutil.RunGit(t, dir, "config", "user.email", "test@example.com")
				testutil.RunGit(t, dir, "config", "commit.gpgsign", "false")
				testutil.RunGit(t, dir, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
				want := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
				assertHeadSHA(t, dir, want)

				testutil.RunGit(t, dir, "pack-refs", "--all")
				assertHeadSHA(t, dir, want)
				testutil.RunGit(t, dir, "checkout", "--detach")
				assertHeadSHA(t, dir, want)

				linked := filepath.Join(t.TempDir(), "linked")
				testutil.RunGit(t, dir, "worktree", "add", "-b", "linked", linked)
				testutil.RunGit(t, linked, "commit", "--allow-empty", "--no-gpg-sign", "-m", "linked only")
				linkedHead := strings.TrimSpace(testutil.RunGit(t, linked, "rev-parse", "HEAD"))
				require.NotEqual(t, want, linkedHead)
				assertHeadSHA(t, linked, linkedHead)
				assertHeadSHA(t, dir, want)
			})
		}
	}
}

func assertHeadSHA(t *testing.T, dir, want string) {
	t.Helper()
	got, err := gitexec.HeadSHA(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestHeadSHA_Failures(t *testing.T) {
	for _, state := range []string{"not a repository", "unborn", "malformed HEAD", "canceled"} {
		t.Run(state, func(t *testing.T) {
			isolateHeadRead(t)
			dir := t.TempDir()
			ctx := t.Context()
			if state != "not a repository" {
				testutil.InitRepo(t, dir)
			}
			switch state {
			case "malformed HEAD":
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("not a reference\n"), 0o600))
			case "canceled":
				testutil.RunGit(t, dir, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := gitexec.HeadSHA(ctx, dir)
			require.Error(t, err)
			require.Empty(t, got, "failed reads must not leak partial Git stdout")
			if state == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestHeadSHA_ExplicitPathNotCWD(t *testing.T) {
	isolateHeadRead(t)
	first, second := t.TempDir(), t.TempDir()
	for _, dir := range []string{first, second} {
		testutil.InitRepo(t, dir)
		testutil.RunGit(t, dir, "commit", "--allow-empty", "--no-gpg-sign", "-m", dir)
	}
	t.Chdir(first)
	firstHead := strings.TrimSpace(testutil.RunGit(t, first, "rev-parse", "HEAD"))
	secondHead := strings.TrimSpace(testutil.RunGit(t, second, "rev-parse", "HEAD"))
	require.NotEqual(t, firstHead, secondHead)
	assertHeadSHA(t, second, secondHead)

	// This user-command helper currently honors explicit native Git selectors.
	// An OpenPath-only replacement would silently change the selected repository.
	t.Setenv("GIT_DIR", filepath.Join(first, ".git"))
	t.Setenv("GIT_WORK_TREE", first)
	assertHeadSHA(t, second, firstHead)
}
