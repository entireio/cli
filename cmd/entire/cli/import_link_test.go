package cli

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

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

	got := resolveImportLinkCommitSHA(repo)
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

	got := resolveImportLinkCommitSHA(repo)
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

	got := resolveImportLinkCommitSHA(repo)
	require.Equal(t, sha, got)
}

// TestResolveImportScanTips_DedupesAndCoversHEAD proves the reconcile scan
// resolves all three tips, deduping when they coincide. HEAD is the one that
// reaches an unmerged feature branch, and origin's tip the one that reaches
// commits merged by someone else — dropping either would silently hide work
// from the scan.
func TestResolveImportScanTips_DedupesAndCoversHEAD(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "one")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "first")
	firstSHA := testutil.GetHeadHash(t, repoDir)

	repo, err := git.PlainOpen(repoDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	// One commit, no origin: the local default-branch tip and HEAD are the same
	// commit, so the result must be a single tip, not the same hash three times.
	require.Equal(t, []plumbing.Hash{plumbing.NewHash(firstSHA)}, resolveImportScanTips(repo))

	// A diverged origin/master adds a second, distinct tip.
	testutil.WriteFile(t, repoDir, "f.txt", "two")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "second")
	secondSHA := testutil.GetHeadHash(t, repoDir)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewRemoteReferenceName("origin", "master"), plumbing.NewHash(firstSHA))))

	// origin's tip comes first (the commit the server already knows), then the
	// local tip. HEAD equals the local tip here and is deduped away.
	require.Equal(t,
		[]plumbing.Hash{plumbing.NewHash(firstSHA), plumbing.NewHash(secondSHA)},
		resolveImportScanTips(repo))
}

// TestResolveImportScanTips_EmptyRepo proves the scan-tip resolver degrades to
// no tips (rather than panicking) on a repo with no commits.
func TestResolveImportScanTips_EmptyRepo(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	// git.PlainInit deliberately: the repo must stay commit-free.
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	require.Empty(t, resolveImportScanTips(repo))
}

// TestResolveImportLinkCommitSHA_EmptyRepo proves the resolver returns "" and
// does not panic on a repo with no commits.
func TestResolveImportLinkCommitSHA_EmptyRepo(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	// git.PlainInit deliberately (not testutil.InitRepo): the repo must stay
	// commit-free, so the helper's user/GPG config is irrelevant here.
	repo, err := git.PlainInit(repoDir, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = repo.Close() })

	got := resolveImportLinkCommitSHA(repo)
	require.Empty(t, got)
}
