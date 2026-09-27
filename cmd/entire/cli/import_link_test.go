package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestResolveImportLinkCommitSHA_SkipsInvalidRefTargets(t *testing.T) {
	t.Parallel()
	for _, invalidTarget := range []string{"missing", "tree"} {
		t.Run(invalidTarget, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			testutil.WriteFile(t, dir, "f.txt", "init")
			testutil.GitAdd(t, dir, "f.txt")
			testutil.GitCommit(t, dir, "init")
			want := testutil.GetHeadHash(t, dir)
			repo, err := git.PlainOpen(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = repo.Close() })
			bad := plumbing.NewHash(strings.Repeat("a", 40))
			if invalidTarget == "tree" {
				commit, err := repo.CommitObject(plumbing.NewHash(want))
				require.NoError(t, err)
				bad = commit.TreeHash
			}
			require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(
				plumbing.NewRemoteReferenceName("origin", "HEAD"), plumbing.NewRemoteReferenceName("origin", "main"))))
			require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
				plumbing.NewRemoteReferenceName("origin", "main"), bad)))
			require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
				plumbing.NewBranchReferenceName("main"), bad)))
			// Both default-branch refs are unusable, but HEAD is a real commit.
			got, err := resolveImportLinkCommitSHA(t.Context(), repo)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// TestResolveImportLinkCommitSHA_LocalDefaultBranchNoOrigin proves that when
// there is no origin remote, the resolver resolves via the local default
// branch arm (testutil.InitRepo checks out master, so GetDefaultBranchName
// returns "master" and the local-branch lookup succeeds). The true HEAD
// fallback is covered by TestResolveImportLinkCommitSHA_HEADWhenNoDefaultBranch.
func TestResolveImportLinkCommitSHA_LocalDefaultBranchNoOrigin(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "init")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	head, err := repo.Head()
	require.NoError(t, err)

	got, err := resolveImportLinkCommitSHA(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, head.Hash().String(), got)
}

// TestResolveImportLinkCommitSHA_PrefersOriginDefaultBranch proves that when
// origin's default branch tip differs from the local branch tip, the
// resolver prefers origin's tip — that's the commit the server already
// knows about.
func TestResolveImportLinkCommitSHA_PrefersOriginDefaultBranch(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "one")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "first")
	firstSHA := testutil.GetHeadHash(t, repoDir)

	testutil.WriteFile(t, repoDir, "f.txt", "two")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "second")
	secondSHA := testutil.GetHeadHash(t, repoDir)

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	// Manually create refs/remotes/origin/main -> first commit, and
	// refs/remotes/origin/HEAD as a symbolic ref pointing at it.
	firstHash := plumbing.NewHash(firstSHA)
	originMainRef := plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "main"), firstHash)
	require.NoError(t, repo.Storer.SetReference(originMainRef))
	originHeadRef := plumbing.NewSymbolicReference(
		plumbing.NewRemoteReferenceName("origin", "HEAD"),
		plumbing.NewRemoteReferenceName("origin", "main"),
	)
	require.NoError(t, repo.Storer.SetReference(originHeadRef))

	// Also create a local main -> second commit, so origin/main and local
	// main genuinely diverge. testutil.InitRepo defaults to `master`, so
	// without this the resolver's local-branch arm is never exercised and
	// the assertion below can't pin the origin-over-local preference order.
	secondHash := plumbing.NewHash(secondSHA)
	localMainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), secondHash)
	require.NoError(t, repo.Storer.SetReference(localMainRef))

	got, err := resolveImportLinkCommitSHA(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, firstSHA, got)
}

// TestResolveImportLinkCommitSHA_HEADWhenNoDefaultBranch proves the HEAD
// fallback: when the default branch name cannot be resolved at all (no
// remotes, and the checked-out branch is neither main nor master), the
// resolver still returns HEAD's commit instead of "".
func TestResolveImportLinkCommitSHA_HEADWhenNoDefaultBranch(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "init")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	sha := testutil.GetHeadHash(t, repoDir)

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	// Rename the branch away from main/master so GetDefaultBranchName
	// returns "" (no origin, and no local main/master to fall back to).
	hash := plumbing.NewHash(sha)
	trunkRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("trunk"), hash)
	require.NoError(t, repo.Storer.SetReference(trunkRef))
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("trunk")),
	))
	require.NoError(t, repo.Storer.RemoveReference(plumbing.NewBranchReferenceName("master")))

	got, err := resolveImportLinkCommitSHA(t.Context(), repo)
	require.NoError(t, err)
	require.Equal(t, sha, got)
}

// TestResolveImportLinkCommitSHA_EmptyRepo proves import cannot proceed with
// no code commit to anchor to.
func TestResolveImportLinkCommitSHA_EmptyRepo(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	got, err := resolveImportLinkCommitSHA(t.Context(), repo)
	require.ErrorContains(t, err, "without a valid anchor commit")
	require.Empty(t, got)
}

// A repository-config failure is not a verdict on any candidate: it fails
// identically for every one, so the loop must stop rather than fall through and
// end up telling the user to fetch code history that is already there. Pins both
// the sentinel wiring and the message, since the whole point is that this
// failure does NOT get the refs' remedy.
func TestResolveImportLinkCommitSHA_ConfigFailureStopsInsteadOfBlamingRefs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	repo, err := gitrepo.OpenPath(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	// Corrupted after opening: the object format is read per validation, so this
	// is what an unreadable config looks like at the moment the anchor is checked.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[[[not ini\n"), 0o600))

	got, err := resolveImportLinkCommitSHA(t.Context(), repo)
	require.Error(t, err)
	require.Empty(t, got)
	require.ErrorIs(t, err, agentimport.ErrAnchorRepoConfig,
		"the sentinel must survive the wrap, or the caller cannot tell this from a bad ref")
	require.NotContains(t, err.Error(), "create a commit or fetch",
		"an unreadable config must not be answered with advice about refs")
}
