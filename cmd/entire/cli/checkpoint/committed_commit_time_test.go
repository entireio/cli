package checkpoint

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

const (
	commitTimeStrategy   = "manual-commit"
	commitTimeTestAuthor = "Test"
	commitTimeTestEmail  = "test@example.com"
)

func TestWriteCommitted_CommitTime(t *testing.T) {
	t.Parallel()

	repo, store := setupCommittedCommitTimeRepo(t)
	commitTime := time.Date(2024, 3, 2, 1, 2, 3, 0, time.UTC)

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: id.MustCheckpointID("a1b2c3d4e5f6"),
		SessionID:    "session-commit-time",
		CreatedAt:    time.Date(2024, 3, 1, 1, 2, 3, 0, time.UTC),
		CommitTime:   commitTime,
		Strategy:     commitTimeStrategy,
		Transcript:   redact.AlreadyRedacted([]byte("transcript line\n")),
		AuthorName:   "Migration",
		AuthorEmail:  "migration@example.com",
	})
	require.NoError(t, err)

	commit := metadataHeadCommit(t, repo)
	require.True(t, commit.Author.When.Equal(commitTime), "author time = %s, want %s", commit.Author.When, commitTime)
	require.True(t, commit.Committer.When.Equal(commitTime), "committer time = %s, want %s", commit.Committer.When, commitTime)
}

func TestWriteCommitted_ZeroCommitTimeUsesCurrentTime(t *testing.T) {
	t.Parallel()

	repo, store := setupCommittedCommitTimeRepo(t)
	createdAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	before := time.Now().Add(-time.Second)

	err := store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: id.MustCheckpointID("b2c3d4e5f6a1"),
		SessionID:    "session-current-time",
		CreatedAt:    createdAt,
		Strategy:     commitTimeStrategy,
		Transcript:   redact.AlreadyRedacted([]byte("transcript line\n")),
		AuthorName:   commitTimeTestAuthor,
		AuthorEmail:  commitTimeTestEmail,
	})
	require.NoError(t, err)
	after := time.Now().Add(time.Second)

	commit := metadataHeadCommit(t, repo)
	require.False(t, commit.Author.When.Equal(createdAt), "zero CommitTime should not reuse CreatedAt as the commit timestamp")
	require.False(t, commit.Author.When.Before(before), "author time = %s, want no earlier than %s", commit.Author.When, before)
	require.False(t, commit.Author.When.After(after), "author time = %s, want no later than %s", commit.Author.When, after)
	require.True(t, commit.Committer.When.Equal(commit.Author.When), "committer time = %s, want author time %s", commit.Committer.When, commit.Author.When)
}

func setupCommittedCommitTimeRepo(t *testing.T) (*git.Repository, *GitStore) {
	t.Helper()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	testutil.WriteFile(t, dir, "README.md", "# Test\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial commit")

	return repo, NewGitStore(repo)
}

func metadataHeadCommit(t *testing.T, repo *git.Repository) *object.Commit {
	t.Helper()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	return commit
}
