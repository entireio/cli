package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// commitPathsWithTrailer commits only paths (git commit -- <paths>, leaving
// the rest of the index staged) with a checkpoint trailer, through the git CLI.
func commitPathsWithTrailer(t *testing.T, dir, checkpointID string, paths ...string) {
	t.Helper()
	msg := "partial commit\n\n" + trailers.CheckpointTrailerKey + ": " + id.MustCheckpointID(checkpointID).String() + "\n"
	testutil.RunGit(t, dir, append([]string{"commit", "-q", "-m", msg, "--"}, paths...)...)
}

// The agent creates two files and the user stages both; one then leaves the
// worktree and the user commits only the other. The file that left is still
// staged, so the next commit adds the agent's blob: the partial commit's
// carry-forward must keep it, and that next commit must link the session.
// Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_KeepsStagedFileThatLeftTheWorktree(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-10-08-staged-carry-forward"
	ctx := context.Background()

	testutil.WriteFile(t, dir, "a.go", "package a\n")
	testutil.WriteFile(t, dir, "b.go", "package b\n")
	saveTestStep(t, s, dir, sid, "a.go", "b.go")
	testutil.GitAdd(t, dir, "a.go")
	testutil.GitAdd(t, dir, "b.go")
	require.NoError(t, os.Remove(filepath.Join(dir, "b.go")))

	commitPathsWithTrailer(t, dir, "a1a1a1a1a1a1", "a.go")
	require.Equal(t, "AD b.go", strings.TrimSpace(testutil.RunGit(t, dir, "status", "--porcelain", "--", "b.go")),
		"fixture: b.go is still staged after the partial commit")
	require.NoError(t, s.PostCommit(ctx))

	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	assert.Contains(t, state.FilesTouched, "b.go", "the still-staged b.go is pending agent work")
	assert.NotEqual(t, touchedFileDeleted, state.TouchedFileHashes["b.go"])

	testutil.RunGit(t, dir, "commit", "-q", "-m", "commit staged b.go")
	assert.True(t, filesOverlapWithContent(ctx, state.TouchedFileHashes, headCommit(t, dir), state.FilesTouched),
		"the second commit holds the agent's b.go and should link the session")
}

// The carry-forward twin of TestSaveStep_IntentToAddThenRemovedIsADeletion: a
// file marked intent-to-add (`git add -N`) and then removed from the worktree
// has no staged blob, so no commit can add it. A partial commit's
// carry-forward must drop it rather than keep it pending after every commit.
// Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_DropsIntentToAddFileThatLeftTheWorktree(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sid := "2026-10-08-intent-to-add-carry-forward"
	ctx := context.Background()

	testutil.WriteFile(t, dir, "a.go", "package a\n")
	testutil.WriteFile(t, dir, "b.go", "package b\n")
	saveTestStep(t, s, dir, sid, "a.go", "b.go")
	testutil.GitAdd(t, dir, "a.go")
	testutil.RunGit(t, dir, "add", "-N", "b.go")
	require.NoError(t, os.Remove(filepath.Join(dir, "b.go")))

	commitPathsWithTrailer(t, dir, "b2b2b2b2b2b2", "a.go")
	require.NoError(t, s.PostCommit(ctx))

	state, err := s.loadSessionState(ctx, sid)
	require.NoError(t, err)
	assert.NotContains(t, state.FilesTouched, "b.go", "an intent-to-add entry is not a staged blob any commit will add")
}
