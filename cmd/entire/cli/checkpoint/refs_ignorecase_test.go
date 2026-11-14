package checkpoint

import (
	"path/filepath"
	"runtime"
	"testing"
	"weak"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// countingRefStorer counts the ref reads a lookup makes.
type countingRefStorer struct {
	storage.Storer

	reads int
}

func (c *countingRefStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	c.reads++
	return c.Storer.Reference(name)
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

// configCountingStorer counts config reads on a filesystem storage.
type configCountingStorer struct {
	*filesystem.Storage

	configReads int
}

func (c *configCountingStorer) Config() (*config.Config, error) {
	c.configReads++
	return c.Storage.Config()
}

func openConfigCountingRepo(t *testing.T, dir string) (*git.Repository, *configCountingStorer) {
	t.Helper()
	plain, err := git.PlainOpen(dir)
	require.NoError(t, err)
	fsStorage, ok := plain.Storer.(*filesystem.Storage)
	require.True(t, ok)
	counting := &configCountingStorer{Storage: fsStorage}
	repo, err := git.Open(counting, nil)
	require.NoError(t, err)
	return repo, counting
}

// The setting is read once per repository directory, not once per opened
// repository: commands open the same repository many times.
func TestFoldsCase_ReadsTheConfigOncePerRepository(t *testing.T) {
	t.Parallel()
	dir, _, _ := shardSpellingRepo(t, "true")
	first, _ := openConfigCountingRepo(t, dir)
	second, counting := openConfigCountingRepo(t, dir)
	counting.configReads = 0 // opening the repository reads its config

	assert.True(t, foldsCase(first))
	assert.True(t, foldsCase(second))
	assert.Zero(t, counting.configReads, "a second opening of the same repository reuses the setting")
}

// A changed setting is read again rather than served from the cache.
func TestFoldsCase_RereadsAChangedSetting(t *testing.T) {
	t.Parallel()
	dir, _, _ := shardSpellingRepo(t, "false")
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	require.False(t, foldsCase(repo))

	testutil.RunGit(t, dir, "config", "core.ignorecase", "true")

	assert.True(t, foldsCase(repo))
}

// With the worktreeConfig extension and a config.worktree file, go-git merges
// the two configs into one without its raw sections. The setting must still be
// read from the repository config rather than crash or be guessed.
func TestFoldsCase_ReadsTheSettingWithAWorktreeConfig(t *testing.T) {
	t.Parallel()
	for _, ignoreCase := range []string{"true", "false"} {
		t.Run(ignoreCase, func(t *testing.T) {
			t.Parallel()
			dir, _, _ := shardSpellingRepo(t, ignoreCase)
			testutil.RunGit(t, dir, "config", "extensions.worktreeConfig", "true")
			testutil.RunGit(t, dir, "config", "--worktree", "core.sparseCheckout", "true")
			repo, err := git.PlainOpen(dir)
			require.NoError(t, err)

			assert.Equal(t, ignoreCase == "true", foldsCase(repo))
		})
	}
}

// A linked worktree with its own config.worktree, opened as commands open it,
// reads the setting from the shared config of the main repository.
func TestFoldsCase_ReadsTheSettingInALinkedWorktree(t *testing.T) {
	t.Parallel()
	for _, ignoreCase := range []string{"true", "false"} {
		t.Run(ignoreCase, func(t *testing.T) {
			t.Parallel()
			dir, _, _ := shardSpellingRepo(t, ignoreCase)
			testutil.RunGit(t, dir, "config", "extensions.worktreeConfig", "true")
			linked := filepath.Join(t.TempDir(), "linked")
			testutil.RunGit(t, dir, "worktree", "add", "-b", "linked", linked)
			testutil.RunGit(t, linked, "config", "--worktree", "core.sparseCheckout", "true")
			repo, err := gitrepo.OpenPath(linked)
			require.NoError(t, err)
			t.Cleanup(func() { _ = repo.Close() })

			assert.Equal(t, ignoreCase == "true", foldsCase(repo))
		})
	}
}

// A storer without a filesystem can only read the merged config, which has no
// raw sections in a worktreeConfig repository. The setting is then unknown, so
// both spellings are read even though the repository does not fold case.
func TestStoredRef_WorktreeConfigWithoutFilesystemReadsBothSpellings(t *testing.T) {
	t.Parallel()
	dir, earlier, _ := shardSpellingRepo(t, "false")
	testutil.RunGit(t, dir, "config", "extensions.worktreeConfig", "true")
	testutil.RunGit(t, dir, "config", "--worktree", "core.sparseCheckout", "true")
	canonical := mustRefName(t, foldableULID)
	testutil.RunGit(t, dir, "update-ref", canonical.String(), earlier)
	repo, counting := openCountingRepo(t, dir)
	counting.reads = 0 // opening the repository reads refs of its own

	name, tip, err := StoredRef(repo, nil, canonical)
	require.NoError(t, err)

	assert.Equal(t, canonical, name)
	assert.Equal(t, plumbing.NewHash(earlier), tip)
	assert.Equal(t, 2, counting.reads, "an unknown setting reads both spellings")
}

// Looking up a checkpoint must not keep its repository in memory.
func TestStoredRef_DoesNotKeepTheRepositoryAlive(t *testing.T) {
	t.Parallel()
	dir, earlier, _ := shardSpellingRepo(t, "false")
	canonical := mustRefName(t, foldableULID)
	testutil.RunGit(t, dir, "update-ref", canonical.String(), earlier)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	_, _, err = StoredRef(repo, nil, canonical)
	require.NoError(t, err)
	collected := weak.Make(repo)

	repo = nil //nolint:ineffassign,wastedassign // drops the last strong reference
	runtime.GC()
	runtime.GC()

	assert.Nil(t, collected.Value(), "the repository must be collectable after a lookup")
}
