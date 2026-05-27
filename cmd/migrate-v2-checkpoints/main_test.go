//nolint:goconst // Repeated CLI flag literals keep argument-list tests readable.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

const (
	oldCheckpointID       = "000000000001"
	mainCheckpointID      = "111111111111"
	featureCheckpointID   = "222222222222"
	featureCheckpointID2  = "333333333333"
	unrelatedCheckpointID = "444444444444"
	testSinceRevision     = "abc123"
	testHeadRevision      = "HEAD"
	testRepoPath          = "/tmp/repo"
	testBaseFilename      = "base.txt"
	testMainFilename      = "main.txt"
	testFeatureFilename   = "feature.txt"
	testFeatureBranchName = "feature"
	testStrategy          = "manual-commit"
	testAuthorName        = "Test"
	testAuthorEmail       = "test@example.com"
	testBranchName        = "main"
	testReviewSkill       = "review-skill"
	testToolUseID         = "toolu_test123"
)

func TestParseOptions(t *testing.T) {
	t.Parallel()

	opts, err := parseOptions([]string{
		"--repo", testRepoPath,
		"--since", testSinceRevision,
		"--head", testHeadRevision,
		"--list",
	})
	require.NoError(t, err)
	require.Equal(t, testRepoPath, opts.repoPath)
	require.Equal(t, testSinceRevision, opts.since)
	require.Equal(t, testHeadRevision, opts.head)
	require.Equal(t, modeList, opts.mode)

	opts, err = parseOptions([]string{"--dry-run", testSinceRevision})
	require.NoError(t, err)
	require.Equal(t, testSinceRevision, opts.since)
	require.Equal(t, modeDryRun, opts.mode)

	_, err = parseOptions([]string{"--since", testSinceRevision, "def456"})
	require.ErrorContains(t, err, "use either --since or positional since commit")

	_, err = parseOptions([]string{"--list", "--apply"})
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

func TestDiscoverCheckpointHistory_SkipsRefsThatDoNotContainSince(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	commitUnrelatedMigrationTestFile(t, fixture.dir)

	checkpoints, err := discoverCheckpointHistory(context.Background(), fixture.repo, discoveryOptions{
		since: fixture.baseHash.String(),
	})
	require.NoError(t, err)

	require.Equal(t, []string{mainCheckpointID, featureCheckpointID, featureCheckpointID2}, discoveredCheckpointIDs(checkpoints))
}

func TestDiscoverCheckpointHistory_HeadMustContainSince(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	unrelatedHash := commitUnrelatedMigrationTestFile(t, fixture.dir)

	_, err := discoverCheckpointHistory(context.Background(), fixture.repo, discoveryOptions{
		since: fixture.baseHash.String(),
		head:  unrelatedHash.String(),
	})
	require.ErrorContains(t, err, "is not an ancestor of --head")
}

func TestDiscoverCheckpointHistory_ExcludesInternalRefs(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	runMigrationGit(t, fixture.dir, "checkout", "-b", paths.MetadataBranchName, fixture.mainHash.String())
	commitMigrationTestFile(t, fixture.dir, "internal.txt", "internal\n",
		"internal checkpoint\n\nEntire-Checkpoint: "+unrelatedCheckpointID)

	repo, err := git.PlainOpen(fixture.dir)
	require.NoError(t, err)
	checkpoints, err := discoverCheckpointHistory(context.Background(), repo, discoveryOptions{
		since: fixture.baseHash.String(),
	})
	require.NoError(t, err)

	require.Equal(t, []string{mainCheckpointID, featureCheckpointID, featureCheckpointID2}, discoveredCheckpointIDs(checkpoints))
}

func TestResolveRevisionRejectsAmbiguousShortCommitPrefix(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	prefixes := map[string]struct{}{}
	ambiguousPrefix := ""
	for i := range 17 {
		hash := commitMigrationTestFile(t, fixture.dir, fmt.Sprintf("ambiguous-%02d.txt", i), fmt.Sprintf("%d\n", i), fmt.Sprintf("ambiguous %d", i))
		prefix := hash.String()[:1]
		if _, exists := prefixes[prefix]; exists {
			ambiguousPrefix = prefix
			break
		}
		prefixes[prefix] = struct{}{}
	}
	require.NotEmpty(t, ambiguousPrefix)

	repo, err := git.PlainOpen(fixture.dir)
	require.NoError(t, err)
	_, err = resolveRevision(repo, ambiguousPrefix)
	require.ErrorContains(t, err, "ambiguous revision")
}

func TestAddCheckpointCommitUsesCommitterTime(t *testing.T) {
	t.Parallel()

	authorTime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	committerTime := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	commit := &object.Commit{
		Hash:      plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Author:    object.Signature{When: authorTime},
		Committer: object.Signature{When: committerTime},
		Message:   "commit\n\nEntire-Checkpoint: " + mainCheckpointID,
	}
	checkpointIndexes := map[string]int{}
	checkpoints := []discoveredCheckpoint{}

	addCheckpointCommit(commit, checkpointIndexes, &checkpoints)

	require.Len(t, checkpoints, 1)
	require.Len(t, checkpoints[0].Commits, 1)
	require.Equal(t, committerTime, checkpoints[0].Commits[0].Date)
}

func TestRunListModeOpensRepoFromSubdirectory(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	subdir := filepath.Join(fixture.dir, "nested")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"--repo", subdir,
		"--since", fixture.baseHash.String(),
		"--head", fixture.mainHash.String(),
		"--list",
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

	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-to-migrate",
		CreatedAt:                 createdAt,
		Strategy:                  testStrategy,
		Branch:                    testBranchName,
		Transcript:                redact.AlreadyRedacted(transcript),
		Prompts:                   []string{"first prompt", "second prompt"},
		FilesTouched:              []string{"main.go"},
		CheckpointsCount:          2,
		AuthorName:                testAuthorName,
		AuthorEmail:               testAuthorEmail,
		Agent:                     agent.AgentTypeClaudeCode,
		Model:                     "claude-test-model",
		TurnID:                    "turn-1",
		CheckpointTranscriptStart: 42,
		CompactTranscriptStart:    9,
		Kind:                      string(session.KindAgentReview),
		ReviewSkills:              []string{testReviewSkill},
		ReviewPrompt:              "review this",
		HasReview:                 true,
	})

	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"--repo", fixture.dir,
		"--since", fixture.baseHash.String(),
		"--head", fixture.mainHash.String(),
		"--apply",
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
	require.Equal(t, testStrategy, content.Metadata.Strategy)
	require.Equal(t, testBranchName, content.Metadata.Branch)
	require.Equal(t, agent.AgentTypeClaudeCode, content.Metadata.Agent)
	require.Equal(t, "claude-test-model", content.Metadata.Model)
	require.Equal(t, "turn-1", content.Metadata.TurnID)
	require.Equal(t, 9, content.Metadata.CheckpointTranscriptStart)
	require.Equal(t, string(session.KindAgentReview), content.Metadata.Kind)
	require.Equal(t, []string{testReviewSkill}, content.Metadata.ReviewSkills)
	require.Equal(t, "review this", content.Metadata.ReviewPrompt)

	ref, err := fixture.repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := fixture.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	require.True(t, commit.Author.When.Equal(createdAt), "author time = %s, want %s", commit.Author.When, createdAt)
}

func TestRunDryRunPlansWithoutWritingV1(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	cpID := id.MustCheckpointID(mainCheckpointID)
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-dry-run",
		Transcript:   redact.AlreadyRedacted([]byte("{\"message\":\"dry run\"}\n")),
	})

	stdout := runMigrationCommand(t, fixture, fixture.mainHash, "--dry-run")
	require.Contains(t, stdout, "Migration plan:")
	require.Contains(t, stdout, "checkpoints eligible for migration: 1")
	require.Contains(t, stdout, "sessions eligible for migration: 1")
	require.Contains(t, stdout, "checkpoints to migrate:")
	require.Contains(t, stdout, mainCheckpointID+" sessions=1 commits="+shortHash(fixture.mainHash))

	summary, err := checkpoint.NewGitStore(fixture.repo).ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.Nil(t, summary)
}

func TestRunApplySkipsExistingV1SessionsAndMigratesMissingSessions(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	cpID := id.MustCheckpointID(mainCheckpointID)
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-existing-v1",
		Transcript:   redact.AlreadyRedacted([]byte("{\"message\":\"from v2 existing\"}\n")),
	})
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "session-v2-missing-from-v1",
		Transcript:   redact.AlreadyRedacted([]byte("{\"message\":\"from v2 new\"}\n")),
	})

	existingTranscript := []byte("{\"message\":\"already v1\"}\n")
	err := checkpoint.NewGitStore(fixture.repo).WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-existing-v1",
		CreatedAt:    time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		Strategy:     testStrategy,
		Branch:       testBranchName,
		Transcript:   redact.AlreadyRedacted(existingTranscript),
		AuthorName:   testAuthorName,
		AuthorEmail:  testAuthorEmail,
	})
	require.NoError(t, err)

	stdout := runMigrationCommand(t, fixture, fixture.mainHash, "--apply")
	require.Contains(t, stdout, "already present v1 sessions: 1")
	require.Contains(t, stdout, "migrated checkpoints: 1")
	require.Contains(t, stdout, "migrated sessions: 1")
	require.Contains(t, stdout, "migrated checkpoint details:")
	require.Contains(t, stdout, mainCheckpointID+" sessions=1 commits="+shortHash(fixture.mainHash))

	v1Store := checkpoint.NewGitStore(fixture.repo)
	summary, err := v1Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.Len(t, summary.Sessions, 2)
	content, err := v1Store.ReadSessionContentByID(context.Background(), cpID, "session-existing-v1")
	require.NoError(t, err)
	require.Equal(t, existingTranscript, content.Transcript)
	require.Equal(t, "session-existing-v1", content.Metadata.SessionID)
	content, err = v1Store.ReadSessionContentByID(context.Background(), cpID, "session-v2-missing-from-v1")
	require.NoError(t, err)
	require.JSONEq(t, `{"message":"from v2 new"}`, string(content.Transcript))
}

func TestRunApplyMigratesTaskMetadata(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	cpID := id.MustCheckpointID(mainCheckpointID)
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "task-session",
		IsTask:       true,
		ToolUseID:    testToolUseID,
		Transcript:   redact.AlreadyRedacted([]byte("{\"message\":\"task\"}\n")),
	})

	stdout := runMigrationCommand(t, fixture, fixture.mainHash, "--apply")
	require.Contains(t, stdout, "migrated sessions: 1")

	v1Store := checkpoint.NewGitStore(fixture.repo)
	content, err := v1Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err)
	require.True(t, content.Metadata.IsTask)
	require.Equal(t, testToolUseID, content.Metadata.ToolUseID)

	ref, err := fixture.repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := fixture.repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	_, err = tree.File(cpID.Path() + "/tasks/" + testToolUseID + "/checkpoint.json")
	require.NoError(t, err)
}

func TestRunApplyHasReviewReflectsOnlyMigratedSessions(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	cpID := id.MustCheckpointID(mainCheckpointID)
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID: cpID,
		SessionID:    "normal-session",
		Transcript:   redact.AlreadyRedacted([]byte("{\"message\":\"normal\"}\n")),
	})
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID:      cpID,
		SessionID:         "review-session-without-raw-transcript",
		Kind:              string(session.KindAgentReview),
		ReviewSkills:      []string{testReviewSkill},
		ReviewPrompt:      "review this",
		HasReview:         true,
		CompactTranscript: []byte("{\"message\":\"compact review only\"}\n"),
	})

	stdout := runMigrationCommand(t, fixture, fixture.mainHash, "--apply")
	require.Contains(t, stdout, "missing raw transcripts: 1")
	require.Contains(t, stdout, "migrated checkpoints: 1")
	require.Contains(t, stdout, "migrated sessions: 1")

	summary, err := checkpoint.NewGitStore(fixture.repo).ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.False(t, summary.HasReview)
	require.Len(t, summary.Sessions, 1)
}

func TestRunDryRunReportsMissingV2MetadataAndRawTranscripts(t *testing.T) {
	t.Parallel()

	fixture := setupMigrationHistoryRepo(t)
	writeTestV2Checkpoint(t, fixture.repo, testV2CheckpointOptions{
		CheckpointID: id.MustCheckpointID(featureCheckpointID),
		SessionID:    "session-missing-raw",
	})

	var stdout bytes.Buffer
	err := run(context.Background(), []string{
		"--repo", fixture.dir,
		"--since", fixture.baseHash.String(),
		"--dry-run",
	}, &stdout)
	require.NoError(t, err)

	require.Contains(t, stdout.String(), "missing v2 checkpoint metadata: 2")
	require.Contains(t, stdout.String(), "missing required v2 session metadata: 0")
	require.Contains(t, stdout.String(), "missing raw transcripts: 1")
	require.Contains(t, stdout.String(), "checkpoints eligible for migration: 0")
	require.Contains(t, stdout.String(), "sessions eligible for migration: 0")
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

func commitUnrelatedMigrationTestFile(t *testing.T, dir string) plumbing.Hash {
	t.Helper()

	runMigrationGit(t, dir, "checkout", "--orphan", "unrelated")
	runMigrationGit(t, dir, "rm", "-rf", ".")
	return commitMigrationTestFile(t, dir, "unrelated.txt", "unrelated\n",
		"unrelated checkpoint\n\nEntire-Checkpoint: "+unrelatedCheckpointID)
}

func runMigrationGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s failed: %s", strings.Join(args, " "), output)
}

func runMigrationCommand(t *testing.T, fixture migrationHistoryFixture, head plumbing.Hash, mode string) string {
	t.Helper()

	args := []string{
		"--repo", fixture.dir,
		"--since", fixture.baseHash.String(),
		"--head", head.String(),
		mode,
	}
	var stdout bytes.Buffer
	err := run(context.Background(), args, &stdout)
	require.NoError(t, err)
	return stdout.String()
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
