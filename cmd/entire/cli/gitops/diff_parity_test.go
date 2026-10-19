package gitops

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

// Compare observable paths with an independent native name-only oracle, rather
// than reusing the raw-output parser that the production implementation uses.
func TestDiffTreeFiles_NativeParity(t *testing.T) {
	for _, change := range []string{"mode", "symlink", "gitlink", "binary", "unusual paths", "rename"} {
		t.Run(change, func(t *testing.T) {
			gitenv.IsolateRepository(t) // production subprocesses also need isolation
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			t.Chdir(dir)
			testutil.WriteFile(t, dir, "original", "original content\n")
			testutil.RunGit(t, dir, "add", "original")
			testutil.RunGit(t, dir, "commit", "--no-gpg-sign", "-m", "initial")
			before := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			blob := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD:original"))
			var expected []string
			switch change {
			case "mode":
				testutil.RunGit(t, dir, "update-index", "--chmod=+x", "original")
				expected = []string{"original"}
			case "symlink":
				// Index-only fixtures exercise symlink tree entries even on Windows.
				testutil.RunGit(t, dir, "update-index", "--add", "--cacheinfo", "120000", blob, "link")
				expected = []string{"link"}
			case "gitlink":
				testutil.RunGit(t, dir, "update-index", "--add", "--cacheinfo", "160000", before, "submodule")
				expected = []string{"submodule"}
			case "binary":
				testutil.WriteFile(t, dir, "original", "binary\x00content")
				testutil.RunGit(t, dir, "add", "original")
				expected = []string{"original"}
			case "unusual paths":
				expected = []string{"space name", "café", "nested/file"}
				if runtime.GOOS != "windows" {
					expected = append(expected, "tab\tname", "line\nname")
				}
				for _, name := range expected {
					testutil.WriteFile(t, dir, name, "new\n")
				}
				testutil.RunGit(t, dir, "add", ".")
			case "rename":
				testutil.RunGit(t, dir, "mv", "original", "renamed")
				expected = []string{"original", "renamed"}
			}
			testutil.RunGit(t, dir, "commit", "--no-gpg-sign", "-m", change)
			after := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			oracle := testutil.RunGit(t, dir, "diff-tree", "--no-commit-id", "--no-renames", "--name-only", "-r", "-z", before, after)
			want := strings.Split(strings.TrimSuffix(oracle, "\x00"), "\x00")
			require.ElementsMatch(t, expected, want, "native baseline")

			indexPath := filepath.Join(dir, ".git", "index")
			indexBefore, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			list, err := DiffTreeFileList(t.Context(), dir, before, after)
			require.NoError(t, err)
			require.ElementsMatch(t, want, list)
			set, err := DiffTreeFiles(t.Context(), dir, before, after)
			require.NoError(t, err)
			require.Len(t, set, len(want))
			for _, name := range want {
				require.Contains(t, set, name)
			}
			indexAfter, err := os.ReadFile(indexPath)
			require.NoError(t, err)
			require.Equal(t, indexBefore, indexAfter, "tree reads must not rewrite the index")
			require.Equal(t, after, strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD")))
		})
	}
}

func TestDiffTreeFiles_ReadFailures(t *testing.T) {
	for _, state := range []string{"unknown revision", "missing object", "canceled"} {
		t.Run(state, func(t *testing.T) {
			gitenv.IsolateRepository(t)
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			t.Chdir(dir)
			testutil.RunGit(t, dir, "commit", "--allow-empty", "--no-gpg-sign", "-m", "initial")
			head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
			target := head
			ctx := t.Context()
			switch state {
			case "unknown revision":
				target = "refs/heads/does-not-exist"
			case "missing object":
				target = strings.Repeat("1", len(head))
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			files, err := DiffTreeFiles(ctx, dir, head, target)
			require.Error(t, err, "failed reads must not masquerade as an empty diff")
			require.Nil(t, files)
			if state == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}
