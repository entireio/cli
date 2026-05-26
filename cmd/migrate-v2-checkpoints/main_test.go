package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

const (
	oldCheckpointID       = "000000000001"
	mainCheckpointID      = "111111111111"
	featureCheckpointID   = "222222222222"
	featureCheckpointID2  = "333333333333"
	testSinceRevision     = "abc123"
	testHeadRevision      = "HEAD"
	testRepoFlag          = "--repo"
	testSinceFlag         = "--since"
	testHeadFlag          = "--head"
	testListFlag          = "--list"
	testDryRunFlag        = "--dry-run"
	testApplyFlag         = "--apply"
	testRepoPath          = "/tmp/repo"
	testBaseFilename      = "base.txt"
	testMainFilename      = "main.txt"
	testFeatureFilename   = "feature.txt"
	testFeatureBranchName = "feature"
)

func TestParseOptions(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions([]string{
		testRepoFlag, testRepoPath,
		testSinceFlag, testSinceRevision,
		testHeadFlag, testHeadRevision,
		testListFlag,
	})
	require.NoError(t, err)
	require.Equal(t, testRepoPath, opts.repoPath)
	require.Equal(t, testSinceRevision, opts.since)
	require.Equal(t, testHeadRevision, opts.head)
	require.Equal(t, modeList, opts.mode)

	opts, err = parseOptions([]string{testDryRunFlag, testSinceRevision})
	require.NoError(t, err)
	require.Equal(t, testSinceRevision, opts.since)
	require.Equal(t, modeDryRun, opts.mode)

	_, err = parseOptions([]string{testSinceFlag, testSinceRevision, "def456"})
	require.ErrorContains(t, err, "use either --since or positional since commit")

	_, err = parseOptions([]string{testListFlag, testApplyFlag})
	require.ErrorContains(t, err, "use only one")
}

func TestDiscoverCheckpointHistory_AllRefsNewerThanSince(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)

	checkpoints, err := discoverCheckpointHistory(context.Background(), fixture.repo, discoveryOptions{
		since: fixture.baseHash.String(),
	})
	require.NoError(t, err)

	require.Equal(t, []string{mainCheckpointID, featureCheckpointID, featureCheckpointID2}, discoveredCheckpointIDs(checkpoints))
	require.Equal(t, []string{shortHash(fixture.mainHash)}, discoveredCommitShortSHAs(t, checkpoints, mainCheckpointID))
	require.Equal(t, []string{shortHash(fixture.featureHash)}, discoveredCommitShortSHAs(t, checkpoints, featureCheckpointID))
	require.Equal(t, []string{shortHash(fixture.featureHash)}, discoveredCommitShortSHAs(t, checkpoints, featureCheckpointID2))
}

func TestDiscoverCheckpointHistory_HeadLimitsScan(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)

	checkpoints, err := discoverCheckpointHistory(context.Background(), fixture.repo, discoveryOptions{
		since: fixture.baseHash.String(),
		head:  fixture.mainHash.String(),
	})
	require.NoError(t, err)

	require.Equal(t, []string{mainCheckpointID}, discoveredCheckpointIDs(checkpoints))
}

func TestRunListModeOpensRepoFromSubdirectory(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	subdir := filepath.Join(fixture.dir, "nested")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		testRepoFlag, subdir,
		testSinceFlag, fixture.baseHash.String(),
		testHeadFlag, fixture.mainHash.String(),
		testListFlag,
	}, &stdout)
	require.NoError(t, err)

	require.Equal(t, mainCheckpointID+" "+shortHash(fixture.mainHash)+"\n", stdout.String())
}

type migrationHistoryFixture struct {
	dir         string
	repo        *git.Repository
	baseHash    plumbing.Hash
	mainHash    plumbing.Hash
	featureHash plumbing.Hash
}

func setupMigrationHistoryRepo(t *testing.T) migrationHistoryFixture {
	t.Helper()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	baseHash := commitMigrationTestFile(t, dir, testBaseFilename, "base\n",
		"base checkpoint\n\nEntire-Checkpoint: "+oldCheckpointID)
	mainHash := commitMigrationTestFile(t, dir, testMainFilename, "main\n",
		"main checkpoint\n\nEntire-Checkpoint: "+mainCheckpointID)

	testutil.GitCheckoutNewBranch(t, dir, testFeatureBranchName)
	featureHash := commitMigrationTestFile(t, dir, testFeatureFilename, "feature\n",
		"feature checkpoint\n\nEntire-Checkpoint: "+featureCheckpointID+"\nEntire-Checkpoint: "+featureCheckpointID2)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	return migrationHistoryFixture{
		dir:         dir,
		repo:        repo,
		baseHash:    baseHash,
		mainHash:    mainHash,
		featureHash: featureHash,
	}
}

func commitMigrationTestFile(t *testing.T, dir, name, content, message string) plumbing.Hash {
	t.Helper()

	testutil.WriteFile(t, dir, name, content)
	testutil.GitAdd(t, dir, name)
	testutil.GitCommit(t, dir, message)
	return plumbing.NewHash(testutil.GetHeadHash(t, dir))
}

func discoveredCheckpointIDs(checkpoints []discoveredCheckpoint) []string {
	ids := make([]string, len(checkpoints))
	for i, checkpoint := range checkpoints {
		ids[i] = checkpoint.ID.String()
	}
	return ids
}

func discoveredCommitShortSHAs(t *testing.T, checkpoints []discoveredCheckpoint, checkpointID string) []string {
	t.Helper()

	for _, checkpoint := range checkpoints {
		if checkpoint.ID.String() != checkpointID {
			continue
		}
		commits := make([]string, len(checkpoint.Commits))
		for i, commit := range checkpoint.Commits {
			commits[i] = commit.ShortSHA
		}
		return commits
	}
	t.Fatalf("checkpoint %s not found in %#v", checkpointID, checkpoints)
	return nil
}
