package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

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

func TestRunApplyMigratesV2CheckpointToV1(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	cpID := id.MustCheckpointID(mainCheckpointID)
	createdAt := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	transcript := []byte("{\"type\":\"assistant\",\"message\":\"migrated\"}\n")

	err := checkpoint.NewV2GitStore(fixture.repo).WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-to-migrate",
		CreatedAt:                 createdAt,
		Strategy:                  "manual-commit",
		Branch:                    "main",
		Transcript:                redact.AlreadyRedacted(transcript),
		Prompts:                   []string{"first prompt", "second prompt"},
		FilesTouched:              []string{"main.go"},
		CheckpointsCount:          2,
		AuthorName:                "Test",
		AuthorEmail:               "test@example.com",
		Agent:                     agent.AgentTypeClaudeCode,
		Model:                     "claude-test-model",
		TurnID:                    "turn-1",
		CheckpointTranscriptStart: 42,
		CompactTranscriptStart:    9,
		Kind:                      string(session.KindAgentReview),
		ReviewSkills:              []string{"review-skill"},
		ReviewPrompt:              "review this",
		HasReview:                 true,
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	err = run(context.Background(), []string{
		testRepoFlag, fixture.dir,
		testSinceFlag, fixture.baseHash.String(),
		testHeadFlag, fixture.mainHash.String(),
		testApplyFlag,
	}, &stdout)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "migrated checkpoints: 1")
	require.Contains(t, stdout.String(), "migrated sessions: 1")

	v1Store := checkpoint.NewGitStore(fixture.repo)
	summary, err := v1Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	require.Equal(t, 2, summary.CheckpointsCount)
	require.Equal(t, []string{"main.go"}, summary.FilesTouched)
	require.True(t, summary.HasReview)

	content, err := v1Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err)
	require.Equal(t, transcript, content.Transcript)
	require.Equal(t, strings.Join([]string{"first prompt", "second prompt"}, checkpoint.PromptSeparator), content.Prompts)
	require.Equal(t, createdAt, content.Metadata.CreatedAt)
	require.Equal(t, "manual-commit", content.Metadata.Strategy)
	require.Equal(t, "main", content.Metadata.Branch)
	require.Equal(t, agent.AgentTypeClaudeCode, content.Metadata.Agent)
	require.Equal(t, "claude-test-model", content.Metadata.Model)
	require.Equal(t, "turn-1", content.Metadata.TurnID)
	require.Equal(t, 0, content.Metadata.CheckpointTranscriptStart)
	require.Equal(t, string(session.KindAgentReview), content.Metadata.Kind)
	require.Equal(t, []string{"review-skill"}, content.Metadata.ReviewSkills)
	require.Equal(t, "review this", content.Metadata.ReviewPrompt)

	ref, err := fixture.repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := fixture.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.True(t, commit.Author.When.Equal(createdAt), "author time = %s, want %s", commit.Author.When, createdAt)
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
