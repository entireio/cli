package gitops

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestDiffTreeFiles_RepositoryLayouts(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, refs := range []string{"files", "reftable"} {
			t.Run(format+"/"+refs, func(t *testing.T) {
				gitenv.IsolateRepository(t)
				root := t.TempDir()
				t.Chdir(t.TempDir())
				testutil.RunGit(t, root, "init", "--object-format="+format, "--ref-format="+refs, "-b", "main")
				testutil.RunGit(t, root, "config", "user.name", "Test")
				testutil.RunGit(t, root, "config", "user.email", "test@example.com")
				testutil.RunGit(t, root, "config", "commit.gpgsign", "false")
				testutil.WriteFile(t, root, "unchanged", "keep\n")
				testutil.WriteFile(t, root, "changed", "before\n")
				testutil.RunGit(t, root, "add", ".")
				testutil.RunGit(t, root, "commit", "--no-gpg-sign", "-m", "initial")
				before := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD"))
				testutil.WriteFile(t, root, "changed", "after\n")
				testutil.RunGit(t, root, "add", ".")
				testutil.RunGit(t, root, "commit", "--no-gpg-sign", "-m", "second")
				after := strings.TrimSpace(testutil.RunGit(t, root, "rev-parse", "HEAD"))
				testutil.RunGit(t, root, "pack-refs", "--all")
				linked := filepath.Join(t.TempDir(), "linked")
				testutil.RunGit(t, root, "worktree", "add", "--detach", linked, before)
				shared := filepath.Join(t.TempDir(), "shared")
				testutil.RunGit(t, root, "clone", "--shared", "--no-checkout", root, shared)
				for _, dir := range []string{root, linked, shared} {
					got, err := DiffTreeFileList(t.Context(), dir, before, after)
					require.NoError(t, err)
					require.Equal(t, []string{"changed"}, got, "objects resolve independently of checked-out HEAD: %s", dir)
					initial, err := DiffTreeFileList(t.Context(), dir, "", before)
					require.NoError(t, err)
					require.ElementsMatch(t, []string{"changed", "unchanged"}, initial)
					// --root only changes root-commit handling; it does not turn a
					// non-root commit into a request to enumerate its entire tree.
					nonRoot, err := DiffTreeFileList(t.Context(), dir, "", after)
					require.NoError(t, err)
					require.Equal(t, []string{"changed"}, nonRoot)
				}
			})
		}
	}
}
