package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// writeStoredCopy plants a session's turn-end stored copy (full.jsonl and
// prompt.txt) under worktreeRoot/.entire/metadata/<session>/, each carrying
// marker so a test can tell which worktree a read came from.
func writeStoredCopy(t *testing.T, worktreeRoot, sessionID, marker string) {
	t.Helper()
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)
	testutil.WriteFile(t, worktreeRoot, filepath.Join(metadataDir, paths.TranscriptFileName),
		`{"type":"human","uuid":"u1","message":{"content":"`+marker+`"}}`+"\n")
	testutil.WriteFile(t, worktreeRoot, filepath.Join(metadataDir, paths.PromptFileName), "prompt from "+marker)
}

// A session that ran in a linked worktree keeps its stored transcript copy in
// THAT worktree's .entire. Condensing it from the main worktree (the sweep,
// doctor) must read the copy from there, or the checkpoint loses its
// transcript once the agent's live file is gone.
func TestCondenseSessionByID_ReadsStoredCopyFromLinkedWorktree(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	mainDir := setupGitRepo(t)
	worktreeDir := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", worktreeDir, "-b", "linked-branch")

	s := &ManualCommitStrategy{}
	sessionID := "linked-worktree-stored-copy"
	metadataDir := paths.SessionMetadataDirFromSessionID(sessionID)

	// The session runs in the linked worktree: its turn-end step records
	// WorktreePath there, and its stored copy lands in that worktree's .entire.
	t.Chdir(worktreeDir)
	paths.ClearWorktreeRootCache()
	writeStoredCopy(t, worktreeDir, sessionID, "linked-worktree-transcript")
	testutil.WriteFile(t, worktreeDir, "work.txt", "agent content")
	liveTranscript := filepath.Join(t.TempDir(), "live.jsonl")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     sessionID,
		NewFiles:      []string{"work.txt"},
		MetadataDir:   metadataDir,
		CommitMessage: "Checkpoint 1",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
		AgentType:     agent.AgentTypeClaudeCode,
	}))
	require.NoError(t, MutateSessionState(t.Context(), sessionID, func(state *SessionState) error {
		// The agent's live transcript is gone; only the stored copy remains.
		state.TranscriptPath = liveTranscript
		return nil
	}))

	// Condense from the main worktree.
	t.Chdir(mainDir)
	paths.ClearWorktreeRootCache()
	state, err := s.loadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, state.WorktreePath)
	mainRoot, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, filepath.Clean(mainRoot), filepath.Clean(state.WorktreePath))

	require.NoError(t, s.CondenseSessionByID(t.Context(), sessionID))

	state, err = s.loadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.False(t, state.LastCheckpointID.IsEmpty(), "condensation should have written a checkpoint")

	repo, err := OpenRepository(t.Context())
	require.NoError(t, err)
	defer repo.Close()
	content, err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).ReadSessionContent(t.Context(), state.LastCheckpointID, 0)
	require.NoError(t, err)
	assert.Contains(t, string(content.Transcript), "linked-worktree-transcript")
	assert.Contains(t, content.Prompts, "prompt from linked-worktree-transcript")

	// The condensation consumed the stored copy, so it is released from the
	// worktree it was read from, as the commit path releases it.
	assertStoredCopyReleased(t, worktreeDir, sessionID)
}

// assertStoredCopyReleased asserts the session's stored prompt.txt and
// full.jsonl are gone from worktreeRoot's .entire.
func assertStoredCopyReleased(t *testing.T, worktreeRoot, sessionID string) {
	t.Helper()
	metadataDir := filepath.Join(worktreeRoot, filepath.FromSlash(paths.SessionMetadataDirFromSessionID(sessionID)))
	for _, name := range []string{paths.PromptFileName, paths.TranscriptFileName} {
		_, err := os.Stat(filepath.Join(metadataDir, name))
		assert.True(t, os.IsNotExist(err), "%s should be released after a commit-less condense", name)
	}
}

// A commit-less condense of a session in the current worktree releases its
// stored copy too. Uses t.Chdir — do NOT add t.Parallel().
func TestCondenseSessionByID_ReleasesStoredCopy(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	dir := setupGitRepo(t)
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	s := &ManualCommitStrategy{}
	sessionID := "release-stored-copy"
	writeStoredCopy(t, dir, sessionID, "current-worktree-transcript")
	testutil.WriteFile(t, dir, "work.txt", "agent content")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     sessionID,
		NewFiles:      []string{"work.txt"},
		MetadataDir:   paths.SessionMetadataDirFromSessionID(sessionID),
		CommitMessage: "Checkpoint 1",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
		AgentType:     agent.AgentTypeClaudeCode,
	}))

	require.NoError(t, s.CondenseSessionByID(t.Context(), sessionID))
	assertStoredCopyReleased(t, dir, sessionID)
}

// Every WorktreePath the gate does not positively accept falls back to the
// current worktree's stored copy, exactly as before cross-worktree reads
// existed: the value comes from a session-state file and is never trusted on
// its own.
func TestStoredSessionRoot_RefusesUntrustedWorktreePaths(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	mainDir := setupGitRepo(t)
	sessionID := "untrusted-worktree-path"
	writeStoredCopy(t, mainDir, sessionID, "main-worktree-copy")

	// The worktree paths below are what git reports for each worktree (the
	// form a session records as WorktreePath), so a refusal is the rule under
	// test and never an unrelated spelling mismatch such as /var vs /private/var
	// on macOS.

	// A directory outside the repository, carrying a stored copy that must
	// never be read.
	unregistered, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	writeStoredCopy(t, unregistered, sessionID, "unregistered-copy")

	// A registered worktree with a genuine stored copy, named relatively.
	linked := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", linked, "-b", "linked-branch")
	linked = gitToplevel(t, linked)
	writeStoredCopy(t, linked, sessionID, "linked-copy")

	t.Chdir(mainDir)
	paths.ClearWorktreeRootCache()
	mainRoot, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	registered, err := gitrepo.ListWorktreePaths(t.Context(), mainRoot)
	require.NoError(t, err)
	require.Contains(t, registered, filepath.ToSlash(linked))
	relative, err := filepath.Rel(mainRoot, linked)
	require.NoError(t, err)
	require.False(t, filepath.IsAbs(relative))

	for _, tc := range []struct {
		name         string
		worktreePath string
	}{
		{name: "empty", worktreePath: ""},
		{name: "unregistered directory", worktreePath: unregistered},
		{name: "relative path to a registered worktree", worktreePath: relative},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &SessionState{SessionID: sessionID, WorktreePath: tc.worktreePath}

			assertReadsMainCopy(t, state)
		})
	}

	// Control: the same registered worktree named absolutely IS read, so the
	// relative case above was refused for how it was named, not for its copy.
	state := &SessionState{SessionID: sessionID, WorktreePath: linked}
	transcript, _ := condensationTranscript(context.Background(), nil, state, storedSessionRootOrNil(context.Background(), state))
	assert.Contains(t, string(transcript), "linked-copy")
}

// A registered worktree whose .entire is a symlink is refused, even though the
// link points at a directory holding a well-formed stored copy: .entire must be
// a real directory.
func TestStoredSessionRoot_RefusesSymlinkedEntireInRegisteredWorktree(t *testing.T) { //nolint:paralleltest // uses t.Chdir
	testutil.SkipWithoutSymlinks(t)
	mainDir := setupGitRepo(t)
	sessionID := "symlinked-entire"
	writeStoredCopy(t, mainDir, sessionID, "main-worktree-copy")

	symlinked := filepath.Join(t.TempDir(), "symlinked")
	testutil.RunGit(t, mainDir, "worktree", "add", symlinked, "-b", "symlinked-branch")
	symlinked = gitToplevel(t, symlinked)
	elsewhere := t.TempDir()
	writeStoredCopy(t, elsewhere, sessionID, "symlinked-copy")
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, paths.EntireDir), filepath.Join(symlinked, paths.EntireDir)))

	t.Chdir(mainDir)
	paths.ClearWorktreeRootCache()
	mainRoot, err := paths.WorktreeRoot(t.Context())
	require.NoError(t, err)
	registered, err := gitrepo.ListWorktreePaths(t.Context(), mainRoot)
	require.NoError(t, err)
	require.Contains(t, registered, filepath.ToSlash(symlinked), "must be refused for its .entire, not for being unregistered")

	assertReadsMainCopy(t, &SessionState{SessionID: sessionID, WorktreePath: symlinked})
}

// assertReadsMainCopy asserts that both condensation reads of state's stored
// copy fell back to the current (main) worktree's copy.
func assertReadsMainCopy(t *testing.T, state *SessionState) {
	t.Helper()
	transcript, path := condensationTranscript(t.Context(), nil, state, storedSessionRootOrNil(t.Context(), state))
	assert.Empty(t, path)
	assert.Contains(t, string(transcript), "main-worktree-copy")

	root, err := storedSessionRoot(t.Context(), state)
	require.NoError(t, err)
	assert.Equal(t, []string{"prompt from main-worktree-copy"}, readPromptsIn(root, state.SessionID))
}

// gitToplevel returns dir's worktree root as git reports it.
func gitToplevel(t *testing.T, dir string) string {
	t.Helper()
	return filepath.FromSlash(strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "--show-toplevel")))
}

// A session ID that is not a valid ID never names a stored-copy file, so a
// traversal in the ID cannot reach a file outside the session's metadata
// directory.
func TestStoredSessionFileName_RejectsInvalidSessionID(t *testing.T) {
	t.Parallel()
	_, err := storedSessionFileName("../../settings", paths.TranscriptFileName)
	require.Error(t, err)

	name, err := storedSessionFileName("valid-session", paths.TranscriptFileName)
	require.NoError(t, err)
	assert.Equal(t, "metadata/valid-session/"+paths.TranscriptFileName, name)
}
