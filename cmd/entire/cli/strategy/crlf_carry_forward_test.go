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
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

const crlfBase = "a\r\nb\r\n"

// setupCRLFRepo commits f.txt with CRLF line endings under core.autocrlf=false,
// then applies configure (attributes or config) so that `git hash-object`
// would normalize the file while `git add` keeps its CRLF (git's index-aware
// rule: a file already committed with CRLF is not converted).
func setupCRLFRepo(t *testing.T, configure func(dir string)) string {
	t.Helper()
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	testutil.RunGit(t, dir, "config", "core.autocrlf", "false")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte(crlfBase), 0o644))
	testutil.RunGit(t, dir, "add", "f.txt")
	testutil.RunGit(t, dir, "commit", "-q", "-m", "crlf file")
	configure(dir)
	return dir
}

func withTextAuto(t *testing.T) func(string) {
	return func(dir string) {
		testutil.WriteFile(t, dir, ".gitattributes", "* text=auto\n")
		testutil.RunGit(t, dir, "add", ".gitattributes")
		testutil.RunGit(t, dir, "commit", "-q", "-m", "text=auto")
	}
}

func withAutoCRLF(t *testing.T) func(string) {
	return func(dir string) {
		testutil.RunGit(t, dir, "config", "core.autocrlf", "true")
	}
}

// agentEditsCRLF records a turn-end step in which the agent rewrote f.txt to
// content.
func agentEditsCRLF(t *testing.T, s *ManualCommitStrategy, dir, sid, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644))
	metadataDir := ".entire/metadata/" + sid
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, metadataDir, paths.TranscriptFileName), []byte(testTranscriptPromptResponse), 0o644))
	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:     sid,
		ModifiedFiles: []string{"f.txt"},
		MetadataDir:   metadataDir,
		CommitMessage: "turn end",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))
}

// commitIndexWithTrailer commits whatever is staged, with a checkpoint trailer.
func commitIndexWithTrailer(t *testing.T, dir, checkpointID string) {
	t.Helper()
	msg := "user commit\n\n" + trailers.CheckpointTrailerKey + ": " + id.MustCheckpointID(checkpointID).String() + "\n"
	testutil.RunGit(t, dir, "commit", "-q", "-m", msg)
}

// stageBlobWithoutFilters stages content for f.txt exactly as given, without
// touching the worktree: what `git add -p` leaves behind.
func stageBlobWithoutFilters(t *testing.T, dir, content string) {
	t.Helper()
	blobFile := filepath.Join(t.TempDir(), "blob")
	require.NoError(t, os.WriteFile(blobFile, []byte(content), 0o644))
	blob := strings.TrimSpace(testutil.RunGit(t, dir, "hash-object", "-w", "--no-filters", blobFile))
	testutil.RunGit(t, dir, "update-index", "--cacheinfo", "100644,"+blob+",f.txt")
}

// A fully committed agent edit of a CRLF file must leave nothing pending, even
// when `git hash-object` (which recorded the hash) normalizes line endings that
// `git add` keeps. Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_CRLFFileFullyCommitted(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*testing.T) func(string)
	}{
		{name: "text=auto added after a CRLF commit", configure: withTextAuto},
		{name: "core.autocrlf=true", configure: withAutoCRLF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupCRLFRepo(t, tc.configure(t))
			s := &ManualCommitStrategy{}
			sid := "2026-10-08-crlf-full"
			ctx := context.Background()

			agentEditsCRLF(t, s, dir, sid, crlfBase+"c\r\n")
			testutil.RunGit(t, dir, "add", "f.txt")
			commitIndexWithTrailer(t, dir, "c1c1c1c1c1c1")
			require.NoError(t, s.PostCommit(ctx))

			state, err := s.loadSessionState(ctx, sid)
			require.NoError(t, err)
			assert.Empty(t, state.FilesTouched, "the agent's edit is fully committed")
			assert.Zero(t, state.StepCount)
		})
	}
}

// Under text=auto, work the commit did not take must still be carried forward:
// a further edit after committing the agent's content, and an `add -p`-style
// partial commit. Uses t.Chdir — do NOT add t.Parallel().
func TestCarryForward_CRLFRemainderKept(t *testing.T) {
	t.Run("dirty remainder after the commit", func(t *testing.T) {
		dir := setupCRLFRepo(t, withTextAuto(t))
		s := &ManualCommitStrategy{}
		sid := "2026-10-08-crlf-dirty"
		ctx := context.Background()

		agentEditsCRLF(t, s, dir, sid, crlfBase+"c\r\n")
		testutil.RunGit(t, dir, "add", "f.txt")
		commitIndexWithTrailer(t, dir, "d1d1d1d1d1d1")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte(crlfBase+"c\r\nd\r\n"), 0o644))
		require.NoError(t, s.PostCommit(ctx))

		state, err := s.loadSessionState(ctx, sid)
		require.NoError(t, err)
		assert.Equal(t, []string{"f.txt"}, state.FilesTouched, "the worktree still differs from the commit")
	})

	t.Run("partial add -p remainder", func(t *testing.T) {
		dir := setupCRLFRepo(t, withTextAuto(t))
		s := &ManualCommitStrategy{}
		sid := "2026-10-08-crlf-partial"
		ctx := context.Background()

		agentEditsCRLF(t, s, dir, sid, crlfBase+"c\r\nd\r\n")
		stageBlobWithoutFilters(t, dir, crlfBase+"c\r\n")
		commitIndexWithTrailer(t, dir, "e1e1e1e1e1e1")
		require.NoError(t, s.PostCommit(ctx))

		state, err := s.loadSessionState(ctx, sid)
		require.NoError(t, err)
		assert.Equal(t, []string{"f.txt"}, state.FilesTouched, "the agent's last line was not committed")
	})
}
