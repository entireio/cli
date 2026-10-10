package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A session's turns can end in a worktree other than its home (the agent was
// resumed there). These tests home a session in the main worktree, then run
// its hooks from a linked one. They use t.Chdir — do NOT add t.Parallel().

// homedSession starts sessionID in mainDir with one turn-end step that
// creates the untracked file home.go, and returns the linked worktree.
func homedSession(t *testing.T, sessionID string) (s *ManualCommitStrategy, mainDir, linkedDir string) {
	t.Helper()
	mainDir = setupGitRepo(t)
	linkedDir = filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", linkedDir, "-b", "linked-branch")
	testutil.WriteFile(t, linkedDir, "linked-only.txt", "advance the linked branch")
	testutil.GitAdd(t, linkedDir, "linked-only.txt")
	testutil.RunGit(t, linkedDir, "commit", "-q", "-m", "linked commit")

	s = &ManualCommitStrategy{}
	chdirWorktree(t, mainDir)
	testutil.WriteFile(t, mainDir, "home.go", "package home // written at home")
	saveStepIn(t, s, sessionID, []string{"home.go"}, nil)
	return s, mainDir, linkedDir
}

func chdirWorktree(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
}

func saveStepIn(t *testing.T, s *ManualCommitStrategy, sessionID string, newFiles, modified []string) {
	t.Helper()
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     sessionID,
		NewFiles:      newFiles,
		ModifiedFiles: modified,
		MetadataDir:   paths.SessionMetadataDirFromSessionID(sessionID),
		CommitMessage: "Checkpoint",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
		AgentType:     agent.AgentTypeClaudeCode,
	}))
}

func mustLoad(t *testing.T, s *ManualCommitStrategy, sessionID string) *SessionState {
	t.Helper()
	state, err := s.loadSessionState(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	return state
}

// Case 1: a turn ending in the linked worktree must not read the home's
// untracked file as deleted (it isn't in that tree), nor move the session's
// base to that tree's HEAD. The turn's own file is still recorded.
func TestGuestTurnEnd_LeavesHomeFilesAndBaseAlone(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "guest-turn-end")
	before := mustLoad(t, s, "guest-turn-end")
	require.NotEqual(t, touchedFileDeleted, before.TouchedFileHashes["home.go"])

	chdirWorktree(t, linkedDir)
	// A turn with no visible changes, then one that writes a file of its own.
	require.NoError(t, s.RecordVanishedUntrackedFiles(t.Context(), "guest-turn-end"))
	testutil.WriteFile(t, linkedDir, "guest.go", "package guest")
	saveStepIn(t, s, "guest-turn-end", []string{"guest.go"}, nil)

	after := mustLoad(t, s, "guest-turn-end")
	assert.Equal(t, before.TouchedFileHashes["home.go"], after.TouchedFileHashes["home.go"], "the home's untracked file was recorded as deleted from another tree")
	assert.Contains(t, after.FilesTouched, "home.go")
	assert.Contains(t, after.FilesTouched, "guest.go")
	assert.NotEmpty(t, after.TouchedFileHashes["guest.go"], "the guest turn's own new file should be hashed")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "the base moved to another tree's HEAD")
	assert.True(t, isSessionHomeWorktree(mustEvalSymlinks(t, mainDir), after), "the session left its home: %s", after.WorktreePath)
}

// The same path edited at home and then in the linked worktree keeps no hash,
// so a commit of either tree's content still links by name; a guest deletion
// leaves the home's record alone.
func TestGuestTurnEnd_SamePathKeepsNoHash(t *testing.T) {
	s, _, linkedDir := homedSession(t, "guest-same-path")
	require.NotEmpty(t, mustLoad(t, s, "guest-same-path").TouchedFileHashes["home.go"])

	chdirWorktree(t, linkedDir)
	testutil.WriteFile(t, linkedDir, "home.go", "package home // the linked tree's version")
	saveStepIn(t, s, "guest-same-path", []string{"home.go"}, nil)

	state := mustLoad(t, s, "guest-same-path")
	_, hashed := state.TouchedFileHashes["home.go"]
	assert.False(t, hashed, "one tree's hash would stop the other tree's commit from linking")
	assert.Contains(t, state.FilesTouched, "home.go")
	assert.Empty(t, guestDeletions(state, []string{"home.go"}), "a guest deletion of a home path must not be recorded")
}

func guestDeletions(state *SessionState, deleted []string) []string {
	_, kept := guestStepHashes(state, nil, deleted)
	return kept
}

// Case 3: the commit-time content gate, prompts and preview read the stored
// copy from the session's home, not the worktree the commit is made in.
func TestStoredCopyReaders_ReadTheHomeFromALinkedWorktree(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "guest-readers")
	writeStoredCopy(t, mainDir, "guest-readers", "home-copy")

	chdirWorktree(t, linkedDir)
	state := mustLoad(t, s, "guest-readers")
	size, ok := storedTranscriptSize(t.Context(), state)
	require.True(t, ok, "the commit-time gate found no stored transcript from the linked worktree")
	assert.Positive(t, size)
	assert.Contains(t, string(readStoredTranscript(t.Context(), state)), "home-copy")
	assert.Equal(t, []string{"prompt from home-copy"}, readPromptsFromFilesystem(t.Context(), state))
	assert.Equal(t, "prompt from home-copy", s.getLastPrompt(t.Context(), state))
}

// Case 4: a turn in the linked worktree writes the stored copy into the
// home's .entire, so the session keeps one copy and no second one appears.
func TestOpenSessionEntireDir_WritesToTheHome(t *testing.T) {
	_, mainDir, linkedDir := homedSession(t, "guest-writes")

	chdirWorktree(t, linkedDir)
	root, err := OpenSessionEntireDir(t.Context(), "guest-writes")
	require.NoError(t, err)
	dirName := entiredir.MustName(paths.SessionMetadataDirFromSessionID("guest-writes"))
	require.NoError(t, osroot.MkdirAllNoSymlink(root, dirName, 0o750))
	name := dirName + "/" + paths.PromptFileName
	require.NoError(t, entiredir.WriteFile(root, name, []byte("turn in the linked tree"), 0o600))

	home := filepath.Join(mainDir, filepath.FromSlash(paths.SessionMetadataDirFromSessionID("guest-writes")), paths.PromptFileName)
	data, err := os.ReadFile(home)
	require.NoError(t, err)
	assert.Equal(t, "turn in the linked tree", string(data))
	_, err = os.Stat(filepath.Join(linkedDir, filepath.FromSlash(paths.SessionMetadataDirFromSessionID("guest-writes"))))
	assert.True(t, os.IsNotExist(err), "a second stored copy appeared in the linked worktree")
}

// A home git no longer lists falls back to the current worktree, as before.
func TestOpenSessionEntireDir_FallsBackWhenTheHomeIsGone(t *testing.T) {
	s, _, linkedDir := homedSession(t, "guest-home-gone")
	require.NoError(t, MutateSessionState(t.Context(), "guest-home-gone", func(state *SessionState) error {
		state.WorktreePath = filepath.Join(t.TempDir(), "not-a-worktree")
		return nil
	}))
	_ = s

	chdirWorktree(t, linkedDir)
	root, err := OpenSessionEntireDir(t.Context(), "guest-home-gone")
	require.NoError(t, err)
	dirName := entiredir.MustName(paths.SessionMetadataDirFromSessionID("guest-home-gone"))
	require.NoError(t, osroot.MkdirAllNoSymlink(root, dirName, 0o750))
	name := dirName + "/" + paths.PromptFileName
	require.NoError(t, entiredir.WriteFile(root, name, []byte("x"), 0o600))
	_, err = os.Stat(filepath.Join(linkedDir, filepath.FromSlash(paths.SessionMetadataDirFromSessionID("guest-home-gone")), paths.PromptFileName))
	assert.NoError(t, err, "the write should land in the current worktree")
}

// A turn starting in the linked worktree keeps the session's branch and base;
// one starting at home still updates them.
func TestGuestTurnStart_KeepsBranchAndBase(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "guest-turn-start")
	before := mustLoad(t, s, "guest-turn-start")

	chdirWorktree(t, linkedDir)
	require.NoError(t, s.InitializeSession(t.Context(), "guest-turn-start", agent.AgentTypeClaudeCode, "", "next prompt", ""))
	after := mustLoad(t, s, "guest-turn-start")
	assert.Equal(t, before.Branch, after.Branch, "a guest turn start captured the linked tree's branch")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "a guest turn start moved the base")

	// Back home, after a commit there, the turn start syncs as before.
	chdirWorktree(t, mainDir)
	testutil.WriteFile(t, mainDir, "later.txt", "later")
	testutil.GitAdd(t, mainDir, "later.txt")
	testutil.RunGit(t, mainDir, "commit", "-q", "-m", "later at home")
	require.NoError(t, s.InitializeSession(t.Context(), "guest-turn-start", agent.AgentTypeClaudeCode, "", "home prompt", ""))
	home := mustLoad(t, s, "guest-turn-start")
	assert.Equal(t, testutil.GetHeadHash(t, mainDir), home.BaseCommit)
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}
