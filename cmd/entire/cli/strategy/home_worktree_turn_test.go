package strategy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
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

// Case 1: a turn ending in the linked worktree records its own file against
// that worktree, never as the home's: the home's untracked file isn't read as
// deleted (it isn't in that tree), and the home's base doesn't move.
func TestGuestTurnEnd_RecordsAgainstItsOwnWorktree(t *testing.T) {
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
	assert.Equal(t, []string{"home.go"}, after.FilesTouched, "the home's record should hold only the home's files")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "the base moved to another tree's HEAD")
	assert.True(t, isSessionHomeWorktree(mustEvalSymlinks(t, mainDir), after), "the session left its home: %s", after.WorktreePath)

	linked := after.OtherWorktrees[linkedKey(t, linkedDir)]
	require.NotNil(t, linked, "the linked worktree's file wasn't recorded for it")
	assert.Equal(t, []string{"guest.go"}, linked.FilesTouched)
	assert.NotEmpty(t, linked.TouchedFileHashes["guest.go"])
	assert.Equal(t, testutil.GetHeadHash(t, linkedDir), linked.BaseCommit)
}

// The same path edited at home and in the linked worktree is two records,
// each with its own tree's content.
func TestGuestTurnEnd_SamePathIsRecordedPerWorktree(t *testing.T) {
	s, _, linkedDir := homedSession(t, "guest-same-path")
	homeHash := mustLoad(t, s, "guest-same-path").TouchedFileHashes["home.go"]
	require.NotEmpty(t, homeHash)

	chdirWorktree(t, linkedDir)
	testutil.WriteFile(t, linkedDir, "home.go", "package home // the linked tree's version")
	saveStepIn(t, s, "guest-same-path", []string{"home.go"}, nil)

	state := mustLoad(t, s, "guest-same-path")
	assert.Equal(t, homeHash, state.TouchedFileHashes["home.go"], "the home's hash was overwritten by another tree's content")
	linked := state.OtherWorktrees[linkedKey(t, linkedDir)]
	require.NotNil(t, linked)
	assert.NotEmpty(t, linked.TouchedFileHashes["home.go"])
	assert.NotEqual(t, homeHash, linked.TouchedFileHashes["home.go"])
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

// An unregistered checkout sharing the repository through commondir has the
// main worktree's empty ID, so for a session homed in the main worktree it
// cannot hold an OtherWorktrees entry of its own (the key is the home's). A
// guest turn ending there must not be recorded as the home's work.
func TestGuestTurnEnd_InUnregisteredCheckoutIsNotRecordedAsHome(t *testing.T) {
	s := &ManualCommitStrategy{}
	mainDir := setupGitRepo(t)
	checkout := unregisteredCheckoutOf(t, mainDir)
	testutil.WriteFile(t, checkout, "checkout-only.txt", "advance the checkout's branch")
	testutil.GitAdd(t, checkout, "checkout-only.txt")
	testutil.RunGit(t, checkout, "commit", "-q", "-m", "checkout commit")

	chdirWorktree(t, mainDir)
	testutil.WriteFile(t, mainDir, "home.go", "package home // written at home")
	saveStepIn(t, s, "guest-unregistered", []string{"home.go"}, nil)
	before := mustLoad(t, s, "guest-unregistered")

	chdirWorktree(t, checkout)
	testutil.WriteFile(t, checkout, "guest.go", "package guest")
	testutil.WriteFile(t, checkout, "checkout-only.txt", "edited in the checkout")
	saveStepIn(t, s, "guest-unregistered", []string{"guest.go"}, []string{"checkout-only.txt"})

	after := mustLoad(t, s, "guest-unregistered")
	assert.Equal(t, []string{"home.go"}, after.FilesTouched, "another checkout's file was recorded as the home's")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "the base moved to another checkout's HEAD")
	assert.Equal(t, before.TouchedFileHashes, after.TouchedFileHashes)
	assert.True(t, isSessionHomeWorktree(mustEvalSymlinks(t, mainDir), after), "the session left its home: %s", after.WorktreePath)
}

// A linked worktree whose marker cannot be created or read gets no record of
// its own, and its step must not fall through to the home's record either:
// that would claim another tree's files and move the home's base. The turn's
// tokens are still the session's, as for a turn that changed no files.
func TestGuestTurnEnd_UnmarkableWorktreeIsNotRecordedAsHome(t *testing.T) {
	s, mainDir, linkedDir := homedSession(t, "guest-unmarkable")
	before := mustLoad(t, s, "guest-unmarkable")
	admin := strings.TrimSpace(testutil.RunGit(t, linkedDir, "rev-parse", "--path-format=absolute", "--git-dir"))
	require.NoError(t, os.WriteFile(filepath.Join(admin, worktreeIncarnationFile), []byte("not a marker"), 0o600))
	_, markErr := worktreeIncarnation(t.Context(), linkedDir, filepath.Base(admin), true)
	require.Error(t, markErr, "precondition: the linked worktree cannot be marked")

	chdirWorktree(t, linkedDir)
	testutil.WriteFile(t, linkedDir, "guest.go", "package guest")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     "guest-unmarkable",
		NewFiles:      []string{"guest.go"},
		MetadataDir:   paths.SessionMetadataDirFromSessionID("guest-unmarkable"),
		CommitMessage: "Checkpoint",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
		AgentType:     agent.AgentTypeClaudeCode,
		TokenUsage:    &agent.TokenUsage{InputTokens: 1000, APICallCount: 1},
	}))

	after := mustLoad(t, s, "guest-unmarkable")
	assert.Equal(t, []string{"home.go"}, after.FilesTouched, "another worktree's file was recorded as the home's")
	assert.Equal(t, before.StepCount, after.StepCount, "the guest step was counted")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "the base moved to another worktree's HEAD")
	assert.Empty(t, after.OtherWorktrees, "the unmarked worktree got a record that can never count")
	assert.True(t, isSessionHomeWorktree(mustEvalSymlinks(t, mainDir), after), "the session left its home: %s", after.WorktreePath)
	require.NotNil(t, after.TokenUsage, "the skipped step's tokens were dropped")
	assert.Equal(t, 1000, after.TokenUsage.InputTokens, "the skipped step's tokens were dropped")
	require.NotNil(t, after.CheckpointTokenUsage)
	assert.Equal(t, 1000, after.CheckpointTokenUsage.InputTokens)
}

// A session homed in a linked worktree records the unregistered checkout
// under the empty ID's key. That record is the checkout's alone: it doesn't
// count in the main worktree (same key), and a step in the main worktree
// neither replaces it nor merges into it.
func TestGuestTurnEnd_EmptyIDEntryBelongsToOneCheckout(t *testing.T) {
	const sid = "empty-id-entry"
	s := &ManualCommitStrategy{}
	mainDir := setupGitRepo(t)
	linkedDir := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", linkedDir, "-b", "linked-branch")
	checkout := unregisteredCheckoutOf(t, mainDir)

	chdirWorktree(t, linkedDir)
	testutil.WriteFile(t, linkedDir, "linked.go", "package linked")
	saveStepIn(t, s, sid, []string{"linked.go"}, nil)

	chdirWorktree(t, checkout)
	testutil.WriteFile(t, checkout, "a.go", "package a // in the checkout")
	saveStepIn(t, s, sid, []string{"a.go"}, nil)

	state := mustLoad(t, s, sid)
	entry := state.OtherWorktrees[session.WorktreeKey("")]
	require.NotNil(t, entry, "the checkout's step wasn't recorded for it")
	assert.Equal(t, []string{"a.go"}, entry.FilesTouched)
	assert.True(t, sameDir(entry.Path, checkout), "entry path = %s", entry.Path)
	assert.True(t, hasWorktreeEntry(t.Context(), state, checkout))
	assert.False(t, hasWorktreeEntry(t.Context(), state, mainDir), "the checkout's record counts in the main worktree")

	chdirWorktree(t, mainDir)
	testutil.WriteFile(t, mainDir, "b.go", "package b // in the main worktree")
	saveStepIn(t, s, sid, []string{"b.go"}, nil)

	after := mustLoad(t, s, sid)
	assert.Equal(t, []string{"linked.go"}, after.FilesTouched, "the home's record changed")
	entry = after.OtherWorktrees[session.WorktreeKey("")]
	require.NotNil(t, entry, "the main worktree's step replaced the checkout's record")
	assert.Equal(t, []string{"a.go"}, entry.FilesTouched, "the main worktree's step merged into the checkout's record")
	assert.True(t, sameDir(entry.Path, checkout), "entry path = %s", entry.Path)
}

// git worktree list never shows an unregistered checkout, and lists the main
// worktree under the same empty ID, so pruning (which runs at every turn end)
// must check the checkout's record where it was made: kept while the checkout
// is there, from any worktree's hook, and dropped once it is gone.
func TestPruneRemovedWorktrees_KeepsUnregisteredCheckoutRecord(t *testing.T) {
	const sid = "prune-unregistered"
	s := &ManualCommitStrategy{}
	mainDir := setupGitRepo(t)
	linkedDir := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, mainDir, "worktree", "add", linkedDir, "-b", "linked-branch")
	checkout := unregisteredCheckoutOf(t, mainDir)

	chdirWorktree(t, linkedDir)
	testutil.WriteFile(t, linkedDir, "linked.go", "package linked")
	saveStepIn(t, s, sid, []string{"linked.go"}, nil)
	chdirWorktree(t, checkout)
	testutil.WriteFile(t, checkout, "a.go", "package a // in the checkout")
	saveStepIn(t, s, sid, []string{"a.go"}, nil)
	require.NotNil(t, mustLoad(t, s, sid).OtherWorktrees[session.WorktreeKey("")], "precondition: the checkout's step was recorded for it")

	for _, hookRoot := range []string{checkout, linkedDir, mainDir} {
		PruneRemovedWorktrees(t.Context(), sid, hookRoot)
		state := mustLoad(t, s, sid)
		entry := state.OtherWorktrees[session.WorktreeKey("")]
		require.NotNil(t, entry, "pruning from %s dropped the live checkout's record", hookRoot)
		assert.Equal(t, []string{"a.go"}, entry.FilesTouched)
		assert.True(t, sameDir(entry.Path, checkout), "pruning from %s moved the record to %s", hookRoot, entry.Path)
		assert.True(t, hasWorktreeEntry(t.Context(), state, checkout), "a commit in the checkout can't link its record")
	}

	chdirWorktree(t, linkedDir)
	require.NoError(t, os.RemoveAll(checkout))
	PruneRemovedWorktrees(t.Context(), sid, linkedDir)
	assert.Nil(t, mustLoad(t, s, sid).OtherWorktrees, "the removed checkout's record kept the session pending")
}

// A linked home moved between two steps of one turn, before turn start could
// follow it, is still the home: its marker says so, and the step is recorded
// there, not skipped.
func TestGuestTurnEnd_HomeMovedMidTurnIsStillHome(t *testing.T) {
	t.Run("linked home moved by git", func(t *testing.T) {
		const sid = "home-moved"
		s := &ManualCommitStrategy{}
		mainDir := setupGitRepo(t)
		linkedDir := filepath.Join(t.TempDir(), "linked")
		testutil.RunGit(t, mainDir, "worktree", "add", linkedDir, "-b", "linked-branch")
		chdirWorktree(t, linkedDir)
		testutil.WriteFile(t, linkedDir, "home.go", "package home")
		saveStepIn(t, s, sid, []string{"home.go"}, nil)

		moved := filepath.Join(t.TempDir(), "linked-moved")
		chdirWorktree(t, mainDir)
		testutil.RunGit(t, mainDir, "worktree", "move", linkedDir, moved)
		chdirWorktree(t, moved)
		testutil.WriteFile(t, moved, "later.go", "package later")
		saveStepIn(t, s, sid, []string{"later.go"}, nil)

		after := mustLoad(t, s, sid)
		assert.Equal(t, []string{"home.go", "later.go"}, after.FilesTouched, "the moved home's step was dropped")
		assert.Equal(t, 2, after.StepCount)
	})
}

// A home with the empty ID has no marker, so a vanished one can't be followed:
// a renamed main worktree looks just like an unregistered checkout whose path
// is gone (a container's path seen from the host). A step in the main
// worktree for a session homed in such a checkout must not be recorded as the
// home's work; its tokens are still kept.
func TestGuestTurnEnd_VanishedEmptyIDHomeIsNotAdopted(t *testing.T) {
	const sid = "vanished-home"
	s := &ManualCommitStrategy{}
	mainDir := setupGitRepo(t)
	checkout := unregisteredCheckoutOf(t, mainDir)
	chdirWorktree(t, checkout)
	testutil.WriteFile(t, checkout, "a.go", "package a // in the checkout")
	saveStepIn(t, s, sid, []string{"a.go"}, nil)
	before := mustLoad(t, s, sid)
	require.True(t, sameDir(before.WorktreePath, checkout), "precondition: homed in the checkout")

	chdirWorktree(t, mainDir)
	require.NoError(t, os.RemoveAll(checkout))
	testutil.WriteFile(t, mainDir, "b.go", "package b // in the main worktree")
	require.NoError(t, s.SaveStep(t.Context(), StepContext{
		SessionID:     sid,
		NewFiles:      []string{"b.go"},
		MetadataDir:   paths.SessionMetadataDirFromSessionID(sid),
		CommitMessage: "Checkpoint",
		AuthorName:    "Test",
		AuthorEmail:   "test@test.com",
		AgentType:     agent.AgentTypeClaudeCode,
		TokenUsage:    &agent.TokenUsage{InputTokens: 1000, APICallCount: 1},
	}))

	after := mustLoad(t, s, sid)
	assert.Equal(t, []string{"a.go"}, after.FilesTouched, "the main worktree's file was recorded as the vanished home's")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "the base moved to the main worktree's HEAD")
	assert.Equal(t, before.StepCount, after.StepCount)
	assert.Equal(t, before.WorktreePath, after.WorktreePath)
	require.NotNil(t, after.TokenUsage)
	assert.Equal(t, 1000, after.TokenUsage.InputTokens)
}

// unregisteredCheckoutOf materializes a checkout of mainDir's current branch on
// its own branch whose .git directory shares mainDir's repository through
// commondir, without a <common>/worktrees registration.
func unregisteredCheckoutOf(t *testing.T, mainDir string) string {
	t.Helper()
	checkout := filepath.Join(t.TempDir(), "workdir")
	testutil.RunGit(t, mainDir, "branch", "checkout-branch")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/checkout-branch\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "commondir"), []byte(filepath.Join(mainDir, ".git")+"\n"), 0o600))
	testutil.RunGit(t, checkout, "read-tree", "-u", "--reset", "HEAD")
	return checkout
}
