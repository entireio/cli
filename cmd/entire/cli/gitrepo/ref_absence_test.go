package gitrepo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestReferenceIsAbsent(t *testing.T) {
	t.Parallel()
	for _, backend := range refCASBackends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			dir, _, _ := backend.init(t)
			repo, err := OpenPath(dir)
			require.NoError(t, err)
			defer repo.Close()
			absent, err := ReferenceIsAbsent(repo, plumbing.NewBranchReferenceName("main"))
			require.NoError(t, err)
			require.False(t, absent)
			absent, err = ReferenceIsAbsent(repo, plumbing.NewBranchReferenceName("missing"))
			require.NoError(t, err)
			require.True(t, absent)
			gitenv.Run(t, dir, "pack-refs", "--all")
			absent, err = ReferenceIsAbsent(repo, plumbing.NewBranchReferenceName("main"))
			require.NoError(t, err)
			require.False(t, absent)
		})
	}
}

func TestReferenceIsAbsent_RefDirectoryIsNotAbsence(t *testing.T) {
	t.Parallel()
	dir, _, _ := initFilesRefCASRepo(t)
	repo, err := OpenPath(dir)
	require.NoError(t, err)
	defer repo.Close()
	ref := plumbing.NewBranchReferenceName("broken")
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git", ref.String()), 0o700))
	absent, err := ReferenceIsAbsent(repo, ref)
	require.Error(t, err)
	require.False(t, absent)
}
