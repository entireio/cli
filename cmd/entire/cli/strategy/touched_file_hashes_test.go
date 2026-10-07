package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitHashObject returns what `git hash-object` reports for a worktree path —
// the blob a commit of the unchanged file would hold.
func gitHashObject(t *testing.T, dir, path string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, dir, "hash-object", "--", path))
}

func TestHashTouchedFiles_HashesRegularFilesLikeGitAdd(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)
	testutil.WriteFile(t, dir, "new.txt", "agent wrote this\n")
	testutil.WriteFile(t, dir, "test.txt", "agent modified\n")

	hashes := hashTouchedFiles(context.Background(), dir, []string{"new.txt", "test.txt", "missing.txt"})

	assert.Equal(t, map[string]string{
		"new.txt":  gitHashObject(t, dir, "new.txt"),
		"test.txt": gitHashObject(t, dir, "test.txt"),
	}, hashes, "missing paths are not hashed")
}

// A clean filter changes what a commit stores; the recorded hash must be the
// committed blob, or every autocrlf user's new files would read as replaced.
func TestHashTouchedFiles_AppliesCleanFilters(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)
	testutil.RunGit(t, dir, "config", "core.autocrlf", "true")
	testutil.WriteFile(t, dir, "crlf.txt", "line one\r\nline two\r\n")

	hashes := hashTouchedFiles(context.Background(), dir, []string{"crlf.txt"})

	testutil.GitAdd(t, dir, "crlf.txt")
	staged := strings.Fields(testutil.RunGit(t, dir, "ls-files", "--stage", "--", "crlf.txt"))
	require.GreaterOrEqual(t, len(staged), 2)
	assert.Equal(t, staged[1], hashes["crlf.txt"])
}

func TestHashTouchedFiles_SkipsSymlinks(t *testing.T) {
	testutil.SkipWithoutSymlinks(t)
	t.Parallel()
	dir := setupGitRepo(t)
	require.NoError(t, os.Symlink("test.txt", filepath.Join(dir, "link.txt")))

	assert.Empty(t, hashTouchedFiles(context.Background(), dir, []string{"link.txt"}),
		"git hash-object follows symlinks, while a commit stores the target path; leave them to name matching")
}

func TestApplyTouchedFileHashes(t *testing.T) {
	t.Parallel()
	state := &SessionState{TouchedFileHashes: map[string]string{
		"kept.txt":     "1111111111111111111111111111111111111111",
		"rehashed.txt": "2222222222222222222222222222222222222222",
		"unhashed.txt": "3333333333333333333333333333333333333333",
	}}

	applyTouchedFileHashes(state,
		[]string{"rehashed.txt", "unhashed.txt", "new.txt"},
		map[string]string{
			"rehashed.txt": "4444444444444444444444444444444444444444",
			"new.txt":      "5555555555555555555555555555555555555555",
		},
		[]string{"gone.txt"},
	)

	assert.Equal(t, map[string]string{
		"kept.txt":     "1111111111111111111111111111111111111111",
		"rehashed.txt": "4444444444444444444444444444444444444444",
		"new.txt":      "5555555555555555555555555555555555555555",
		"gone.txt":     touchedFileDeleted,
	}, state.TouchedFileHashes, "a later step overwrites, a path it could not hash loses its stale entry")

	_, recorded, deleted := recordedFileHash(state.TouchedFileHashes, "gone.txt")
	assert.True(t, recorded)
	assert.True(t, deleted)
	_, recorded, _ = recordedFileHash(state.TouchedFileHashes, "unhashed.txt")
	assert.False(t, recorded)
}

func TestDropPhantomFilesTouched_OnlyDropsThisStepsMissingPaths(t *testing.T) {
	t.Parallel()
	dir := setupGitRepo(t)
	testutil.WriteFile(t, dir, "real.txt", "x")
	state := &SessionState{
		FilesTouched: []string{"deleted.txt", "earlier-missing.txt", "phantom.txt", "real.txt"},
		TouchedFileHashes: map[string]string{
			"deleted.txt":         touchedFileDeleted,
			"earlier-missing.txt": "1111111111111111111111111111111111111111",
		},
	}

	dropPhantomFilesTouched(dir, state, []string{"deleted.txt", "phantom.txt", "real.txt"})

	assert.Equal(t, []string{"deleted.txt", "earlier-missing.txt", "real.txt"}, state.FilesTouched,
		"a missing path from this step that is not a recorded deletion is a phantom; an earlier step's missing file (e.g. stashed) stays")
}

func TestPruneTouchedFileHashes(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		FilesTouched: []string{"a.txt"},
		TouchedFileHashes: map[string]string{
			"a.txt": "1111111111111111111111111111111111111111",
			"b.txt": "2222222222222222222222222222222222222222",
		},
	}
	pruneTouchedFileHashes(state)
	assert.Equal(t, map[string]string{"a.txt": "1111111111111111111111111111111111111111"}, state.TouchedFileHashes)

	state.FilesTouched = nil
	pruneTouchedFileHashes(state)
	assert.Nil(t, state.TouchedFileHashes)
}

// SaveStep is where turn-end hashes are recorded. Uses t.Chdir — do NOT add
// t.Parallel().
func TestSaveStep_RecordsTouchedFileHashes(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sessionID := "2026-10-06-touched-hashes"

	testutil.WriteFile(t, dir, "test.txt", "agent modified\n")
	testutil.WriteFile(t, dir, "new.txt", "agent created\n")
	metadataDir := ".entire/metadata/" + sessionID
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, metadataDir, paths.TranscriptFileName), []byte(testTranscriptPromptResponse), 0o644))

	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:     sessionID,
		ModifiedFiles: []string{"test.txt", "phantom.txt"},
		NewFiles:      []string{"new.txt"},
		MetadataDir:   metadataDir,
		CommitMessage: "turn end",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, 1, state.StepCount)
	assert.Equal(t, []string{"new.txt", "test.txt"}, state.FilesTouched, "the phantom path is dropped at turn end")
	assert.Equal(t, map[string]string{
		"new.txt":  gitHashObject(t, dir, "new.txt"),
		"test.txt": gitHashObject(t, dir, "test.txt"),
	}, state.TouchedFileHashes)
}

// Task-record completion merges the subagent's files without hashing them; a
// hash an earlier step recorded for one of those paths is stale (the subagent
// may have rewritten the file) and must fall back to name matching.
func TestApplyTaskRecordCompletion_DropsStaleTouchedFileHashes(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		FilesTouched: []string{"kept.txt", "rewritten.txt"},
		TouchedFileHashes: map[string]string{
			"kept.txt":      "1111111111111111111111111111111111111111",
			"rewritten.txt": "2222222222222222222222222222222222222222",
		},
	}
	state.AddTaskRecord(session.TaskRecord{ToolUseID: "toolu_1", StartedAt: time.Now()})

	require.NoError(t, applyTaskRecordCompletion(state, session.TaskRecord{
		ToolUseID: "toolu_1",
		Files:     []string{"rewritten.txt", "added.txt"},
	}))

	assert.Equal(t, []string{"added.txt", "kept.txt", "rewritten.txt"}, state.FilesTouched)
	assert.Equal(t, map[string]string{"kept.txt": "1111111111111111111111111111111111111111"}, state.TouchedFileHashes)
}

func TestMergeUnhashedFilesTouched_ClearsEmptyMap(t *testing.T) {
	t.Parallel()
	state := &SessionState{
		FilesTouched:      []string{"a.txt"},
		TouchedFileHashes: map[string]string{"a.txt": touchedFileDeleted},
	}
	MergeUnhashedFilesTouched(state, []string{"a.txt"})
	assert.Equal(t, []string{"a.txt"}, state.FilesTouched)
	assert.Nil(t, state.TouchedFileHashes)
}

// A step whose every changed path is a phantom (named by the transcript, absent
// from the worktree) and that deletes nothing records no work: it must not
// count a step, or the session stays pending with nothing a commit can match.
// Uses t.Chdir — do NOT add t.Parallel().
func TestSaveStep_PhantomOnlyStepIsSkipped(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)
	s := &ManualCommitStrategy{}
	sessionID := "2026-10-07-phantom-only"

	metadataDir := ".entire/metadata/" + sessionID
	require.NoError(t, os.MkdirAll(filepath.Join(dir, metadataDir), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, metadataDir, paths.TranscriptFileName), []byte(testTranscriptPromptResponse), 0o644))

	require.NoError(t, s.SaveStep(context.Background(), StepContext{
		SessionID:     sessionID,
		ModifiedFiles: []string{"never/created.go"},
		NewFiles:      []string{"also/missing.go"},
		MetadataDir:   metadataDir,
		CommitMessage: "turn end",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
	}))

	state, err := s.loadSessionState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Zero(t, state.StepCount)
	assert.Empty(t, state.FilesTouched)
	assert.False(t, state.HasPendingWork())
}
