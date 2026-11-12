package checkpoint

import (
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// countingRefStorer counts the ref reads a lookup makes.
type countingRefStorer struct {
	storage.Storer
	reads int
}

func (c *countingRefStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	c.reads++
	return c.Storer.Reference(name) //nolint:wrapcheck // test pass-through
}

// shardSpellingRepo is a repository with core.ignorecase set to ignoreCase and
// two commits, the second a child of the first.
func shardSpellingRepo(t *testing.T, ignoreCase string) (dir, earlier, later string) {
	t.Helper()
	dir = t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.RunGit(t, dir, "config", "core.ignorecase", ignoreCase)
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "earlier")
	earlier = testutil.RunGit(t, dir, "rev-parse", "HEAD")[:40]
	testutil.RunGit(t, dir, "commit", "--allow-empty", "-m", "later")
	later = testutil.RunGit(t, dir, "rev-parse", "HEAD")[:40]
	return dir, earlier, later
}

func openCountingRepo(t *testing.T, dir string) (*git.Repository, *countingRefStorer) {
	t.Helper()
	plain, err := git.PlainOpen(dir)
	require.NoError(t, err)
	counting := &countingRefStorer{Storer: plain.Storer}
	repo, err := git.Open(counting, nil)
	require.NoError(t, err)
	return repo, counting
}

// Splits only form on a case-insensitive filesystem, where git sets
// core.ignorecase. Elsewhere a lookup whose canonical name resolves reads one
// ref, not two.
func TestStoredRef_CaseSensitiveRepoReadsOneSpelling(t *testing.T) {
	t.Parallel()
	dir, earlier, later := shardSpellingRepo(t, "false")
	canonical := mustRefName(t, foldableULID)
	folded, ok := FoldedRefName(foldableULID)
	require.True(t, ok)
	// Packed in between, so each spelling is its own entry on any filesystem.
	testutil.RunGit(t, dir, "update-ref", canonical.String(), earlier)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	testutil.RunGit(t, dir, "update-ref", folded.String(), later)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	repo, counting := openCountingRepo(t, dir)
	counting.reads = 0 // opening the repository reads refs of its own

	name, tip, err := StoredRef(repo, nil, canonical)
	require.NoError(t, err)

	assert.Equal(t, canonical, name)
	assert.Equal(t, plumbing.NewHash(earlier), tip)
	assert.Equal(t, 1, counting.reads, "a case-sensitive repository reads the canonical name only")
}

// A ref stored only under the other spelling still resolves in a
// case-sensitive repository, for one copied from a case-insensitive one.
func TestStoredRef_CaseSensitiveRepoFindsTheOtherSpellingAlone(t *testing.T) {
	t.Parallel()
	dir, earlier, _ := shardSpellingRepo(t, "false")
	canonical := mustRefName(t, foldableULID)
	folded, ok := FoldedRefName(foldableULID)
	require.True(t, ok)
	testutil.RunGit(t, dir, "update-ref", folded.String(), earlier)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	repo, _ := openCountingRepo(t, dir)

	name, tip, err := StoredRef(repo, nil, canonical)
	require.NoError(t, err)

	assert.Equal(t, folded, name)
	assert.Equal(t, plumbing.NewHash(earlier), tip)
}

// Where splits form, both spellings are still read and the later version wins.
func TestStoredRef_CaseInsensitiveRepoUsesTheLaterVersion(t *testing.T) {
	t.Parallel()
	dir, earlier, later := shardSpellingRepo(t, "true")
	canonical := mustRefName(t, foldableULID)
	folded, ok := FoldedRefName(foldableULID)
	require.True(t, ok)
	testutil.RunGit(t, dir, "update-ref", canonical.String(), earlier)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	testutil.RunGit(t, dir, "update-ref", folded.String(), later)
	testutil.RunGit(t, dir, "pack-refs", "--all")
	repo, _ := openCountingRepo(t, dir)

	name, tip, err := StoredRef(repo, nil, canonical)
	require.NoError(t, err)

	assert.Equal(t, folded, name)
	assert.Equal(t, plumbing.NewHash(later), tip)
}
