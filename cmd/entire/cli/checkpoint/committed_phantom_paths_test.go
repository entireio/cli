package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteCommitted_EmptyTranscript_NoPhantomPaths(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	readmeFile := filepath.Join(tempDir, "README.md")
	require.NoError(t, os.WriteFile(readmeFile, []byte("# Test"), 0o644))
	_, err = wt.Add("README.md")
	require.NoError(t, err)
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	store := NewGitStore(repo)
	cpID := id.MustCheckpointID("d4e5f6a1b2c3")

	// Write a checkpoint with NO transcript
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-no-transcript",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Read back the checkpoint summary and verify no phantom paths
	summary, err := store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)

	sess := summary.Sessions[0]
	assert.Empty(t, sess.Transcript, "transcript path should be empty when no transcript was written")
	assert.Empty(t, sess.ContentHash, "content hash path should be empty when no transcript was written")
	assert.NotEmpty(t, sess.Metadata, "metadata path should always be set")
}

func TestWriteCommitted_WithTranscript_PathsPopulated(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	testutil.InitRepo(t, tempDir)
	repo, err := git.PlainOpen(tempDir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	readmeFile := filepath.Join(tempDir, "README.md")
	require.NoError(t, os.WriteFile(readmeFile, []byte("# Test"), 0o644))
	_, err = wt.Add("README.md")
	require.NoError(t, err)
	_, err = wt.Commit("Initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	})
	require.NoError(t, err)

	store := NewGitStore(repo)
	cpID := id.MustCheckpointID("e5f6a1b2c3d4")

	// Write a checkpoint WITH a transcript
	err = store.WriteCommitted(context.Background(), WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-with-transcript",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript line 1\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Read back and verify paths are populated
	summary, err := store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)

	sess := summary.Sessions[0]
	assert.NotEmpty(t, sess.Transcript, "transcript path should be set when transcript was written")
	assert.NotEmpty(t, sess.ContentHash, "content hash path should be set when transcript was written")
	assert.NotEmpty(t, sess.Metadata, "metadata path should always be set")
}
