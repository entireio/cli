package gitrepo

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestCommitAtReference_Reftable(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			dir, head := initReftableRepoWithFormat(t, format, "file", "contents\n")
			gitenv.Run(t, dir, "tag", "--no-sign", "-a", "outer", "-m", "tag")
			gitenv.Run(t, dir, "tag", "--no-sign", "-a", "nested", "outer", "-m", "nested")
			repo, err := OpenPath(dir)
			require.NoError(t, err)
			defer repo.Close()
			for _, ref := range []plumbing.ReferenceName{plumbing.HEAD, "refs/tags/nested"} {
				commit, err := CommitAtReference(t.Context(), repo, ref)
				require.NoError(t, err)
				require.Equal(t, head, commit.Hash.String())
			}
		})
	}
}
