package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Uses t.Chdir — do NOT add t.Parallel().

// twoWorktrees returns a repository and a linked worktree beside it.
func twoWorktrees(t *testing.T) (mainDir, linkedDir string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	mainDir = setupSessionMatchRepo(t)
	linkedDir = filepath.Join(resolvedTempDir(t), "linked")
	createSessionMatchWorktree(t, mainDir, linkedDir, "linked")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, linkedDir) })
	return mainDir, linkedDir
}

func linkedKey(t *testing.T, dir string) string {
	t.Helper()
	worktreeID, err := paths.GetWorktreeID(dir)
	require.NoError(t, err)
	return session.WorktreeKey(worktreeID)
}

// mark returns the marker a record of edits in the linked worktree at dir is
// made under (creating it), as recording an edit there would.
func mark(t *testing.T, dir string) string {
	t.Helper()
	worktreeID, err := paths.GetWorktreeID(dir)
	require.NoError(t, err)
	marker, err := MarkWorktree(context.Background(), dir, worktreeID)
	require.NoError(t, err)
	require.NotEmpty(t, marker)
	return marker
}

func TestEnterWorktreeView(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	key := linkedKey(t, linkedDir)
	state := &SessionState{
		SessionID:         "view",
		WorktreePath:      mainDir,
		BaseCommit:        "home-base",
		FilesTouched:      []string{"home.go"},
		TouchedFileHashes: map[string]string{"home.go": "h"},
		OtherWorktrees: map[string]*session.WorktreeWork{
			key: {Path: linkedDir, BaseCommit: "linked-base", FilesTouched: []string{"a.go", "b.go"}, TouchedFileHashes: map[string]string{"a.go": "ha"}, Incarnation: mark(t, linkedDir)},
		},
	}

	_, ok := enterWorktreeView(context.Background(), state, mainDir)
	assert.False(t, ok, "the home has no view of its own")

	leave, ok := enterWorktreeView(context.Background(), state, linkedDir)
	require.True(t, ok)
	assert.Equal(t, []string{"a.go", "b.go"}, state.FilesTouched)
	assert.Equal(t, "linked-base", state.BaseCommit)
	assert.Equal(t, linkedDir, state.WorktreePath)
	assert.True(t, state.HasPendingFiles())
	assert.Equal(t, 3, state.PendingFileCount(), "the home's files still count while in the view")

	// A commit in the linked worktree took a.go.
	state.FilesTouched = []string{"b.go"}
	state.BaseCommit = "linked-new"
	leave()
	assert.Equal(t, []string{"home.go"}, state.FilesTouched)
	assert.Equal(t, "home-base", state.BaseCommit)
	assert.Equal(t, mainDir, state.WorktreePath)
	assert.Equal(t, []string{"b.go"}, state.OtherWorktrees[key].FilesTouched)
	assert.Equal(t, "linked-new", state.OtherWorktrees[key].BaseCommit)
	assert.Equal(t, mark(t, linkedDir), state.OtherWorktrees[key].Incarnation, "the view dropped the entry's marker")
	assert.Len(t, state.OtherWorktrees, 1)

	// Once that worktree has nothing left, its entry goes.
	leave, ok = enterWorktreeView(context.Background(), state, linkedDir)
	require.True(t, ok)
	state.FilesTouched = nil
	leave()
	assert.Nil(t, state.OtherWorktrees)
}

// homedWithEntry saves a session homed in mainDir that recorded a.go in
// linkedDir, plus a session homed in linkedDir with nothing pending, so the
// linked worktree's own sessions are an exact match there.
func homedWithEntry(t *testing.T, mainDir, linkedDir string) {
	t.Helper()
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, linkedDir, &SessionState{SessionID: "linked-own", WorktreePath: linkedDir})
	require.NoError(t, MutateSessionState(ctx, "linked-own", func(st *SessionState) error {
		st.Phase = session.PhaseIdle
		return nil
	}))
	testutil.WriteFile(t, linkedDir, "a.go", "package a\n")
	hashes := hashTouchedFiles(ctx, linkedDir, []string{"a.go"})
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "from-main",
		WorktreePath: mainDir,
		FilesTouched: []string{"home.go"},
		StepCount:    1,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"a.go"}, TouchedFileHashes: hashes, Incarnation: mark(t, linkedDir)},
		},
	})
	require.NoError(t, MutateSessionState(ctx, "from-main", func(st *SessionState) error {
		st.Phase = session.PhaseIdle
		return nil
	}))
}

// A session that recorded edits in a worktree joins that worktree's linking
// set — marked, and seen through its record of that worktree — even when the
// worktree has sessions of its own.
func TestCommitLinking_IncludesSessionsThatEditedTheWorktree(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	homedWithEntry(t, mainDir, linkedDir)

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	sessions, err := (&ManualCommitStrategy{}).findSessionsForCommitLinking(context.Background(), linkedDir)
	require.NoError(t, err)
	var fromMain *SessionState
	for _, st := range sessions {
		if st.SessionID == "from-main" {
			fromMain = st
		}
	}
	require.NotNil(t, fromMain, "the session that edited this worktree is not in its linking set")
	assert.True(t, fromMain.LinkedByWorktreeEntry)
	assert.Equal(t, []string{"a.go"}, fromMain.FilesTouched, "it should be judged by what it recorded here")
}

// Linked only by its record of a worktree, a session stamps a commit there
// only when the commit holds those files.
func TestPrepareCommitMsg_SessionFromAnotherWorktreeNeedsItsFiles(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	homedWithEntry(t, mainDir, linkedDir)

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	commit := func(file string) string {
		t.Helper()
		msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
		require.NoError(t, os.WriteFile(msg, []byte("commit "+file+"\n"), 0o600))
		testutil.GitAdd(t, linkedDir, file)
		require.NoError(t, (&ManualCommitStrategy{}).PrepareCommitMsg(context.Background(), msg, "message"))
		data, err := os.ReadFile(msg)
		require.NoError(t, err)
		testutil.RunGit(t, linkedDir, "reset", "-q")
		return string(data)
	}

	testutil.WriteFile(t, linkedDir, "other.go", "package other\n")
	assert.NotContains(t, commit("other.go"), "Entire-Checkpoint", "a commit without the session's files in this worktree must not link it")
	assert.Contains(t, commit("a.go"), "Entire-Checkpoint", "a commit of the session's file in this worktree should link it")
}

// An ACTIVE session linked only by its record of a worktree isn't stamped on
// the no-TTY fast path by a commit there that lacks its files: a script or GUI
// client committing in that worktree mid-turn must not take its trailer.
func TestFastPath_SessionFromAnotherWorktreeNeedsItsFiles(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	homedWithEntry(t, mainDir, linkedDir)
	require.NoError(t, MutateSessionState(context.Background(), "from-main", func(st *SessionState) error {
		st.Phase = session.PhaseActive
		st.Owner = nil // not the committing process's ancestor
		return nil
	}))

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msg, []byte("unrelated\n"), 0o600))
	testutil.WriteFile(t, linkedDir, "other.go", "package other\n")
	testutil.GitAdd(t, linkedDir, "other.go")
	require.NoError(t, (&ManualCommitStrategy{}).PrepareCommitMsg(context.Background(), msg, "message"))
	data, err := os.ReadFile(msg)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Entire-Checkpoint")
}

// Matched by the sibling-worktree fallback (the worktree has no sessions of
// its own), a session that recorded edits there is still held to them on the
// no-TTY fast path, unless ancestry identifies it as the committer.
func TestFastPath_FallbackMatchedSessionNeedsItsFiles(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	testutil.WriteFile(t, linkedDir, "a.go", "package a\n")
	saveSessionMatchState(ctx, t, &ManualCommitStrategy{}, mainDir, &SessionState{
		SessionID:    "from-main",
		WorktreePath: mainDir,
		StepCount:    1,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"a.go"}, TouchedFileHashes: hashTouchedFiles(ctx, linkedDir, []string{"a.go"}), Incarnation: mark(t, linkedDir)},
		},
	})

	t.Chdir(linkedDir)
	clearSessionMatchCaches()
	msg := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msg, []byte("unrelated\n"), 0o600))
	testutil.WriteFile(t, linkedDir, "other.go", "package other\n")
	testutil.GitAdd(t, linkedDir, "other.go")
	require.NoError(t, (&ManualCommitStrategy{}).PrepareCommitMsg(ctx, msg, "message"))
	data, err := os.ReadFile(msg)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "Entire-Checkpoint")
}

// A background task's SubagentStop must not claim a file another session
// recorded in the task's worktree from elsewhere: it counts as recorded, as
// one in the FilesTouched of a session homed there does.
func TestLoadBackgroundClaimContext_CountsEditsRecordedFromElsewhere(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, linkedDir, &SessionState{
		SessionID:    "task-owner",
		WorktreePath: linkedDir,
		TaskRecords:  []session.TaskRecord{{ToolUseID: "bg", Background: true, StartedAt: time.Now()}},
	})
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "from-main",
		WorktreePath: mainDir,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"theirs.go"}, Incarnation: mark(t, linkedDir)},
		},
	})

	claims, ok, err := LoadBackgroundClaimContext(ctx, "task-owner", "bg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, claims.Recorded, "theirs.go", "the task may claim another session's edit in its worktree")
}

// The pending preview (checkpoint list --pending) lists a session's files in
// every worktree, not only its home's.
func TestPreviewSession_ListsFilesInOtherWorktrees(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	chdirWorktree(t, mainDir)
	preview := previewSession(context.Background(), &SessionState{
		SessionID:    "preview",
		WorktreePath: mainDir,
		FilesTouched: []string{"home.go"},
		StepCount:    2,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"a.go"}},
		},
	})
	assert.Equal(t, []string{"a.go", "home.go"}, preview.FilesTouched)
}

// An entry from a worktree git has since removed and re-added under the same
// ID records nothing in the new one: a background task there may claim it.
func TestLoadBackgroundClaimContext_IgnoresARemovedWorktreesEntry(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	s := &ManualCommitStrategy{}
	saveSessionMatchState(ctx, t, s, mainDir, &SessionState{
		SessionID:    "from-main",
		WorktreePath: mainDir,
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"stale.go"}, Incarnation: mark(t, linkedDir)},
		},
	})
	readdLinked(t, mainDir, linkedDir)
	saveSessionMatchState(ctx, t, s, linkedDir, &SessionState{
		SessionID:    "task-owner",
		WorktreePath: linkedDir,
		TaskRecords:  []session.TaskRecord{{ToolUseID: "bg", Background: true, StartedAt: time.Now()}},
	})

	claims, ok, err := LoadBackgroundClaimContext(ctx, "task-owner", "bg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotContains(t, claims.Recorded, "stale.go", "a removed worktree's entry kept the task from claiming its own file")
}

// A commit in another worktree, judged through the session's record of it,
// still reads the stored transcript and prompts from the home's .entire: the
// view names the committing worktree, not where the copy lives.
func TestStoredSessionRoot_InWorktreeViewReadsTheHomesCopy(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	ctx := context.Background()
	sessionID := "view-stored-copy"
	writeStoredCopy(t, gitToplevel(t, mainDir), sessionID, "home-copy")
	writeStoredCopy(t, linkedDir, sessionID, "linked-copy")
	state := &SessionState{
		SessionID:    sessionID,
		WorktreePath: gitToplevel(t, mainDir),
		FilesTouched: []string{"home.go"},
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"a.go"}, Incarnation: mark(t, linkedDir)},
		},
	}

	chdirWorktree(t, linkedDir)
	leave, ok := enterWorktreeView(ctx, state, gitToplevel(t, linkedDir))
	require.True(t, ok)
	root, err := storedSessionRoot(ctx, state)
	require.NoError(t, err)
	assert.Equal(t, []string{"prompt from home-copy"}, readPromptsIn(root, sessionID), "the view read the committing worktree's copy")
	leave()
	assert.Empty(t, state.ViewHomePath)
}
