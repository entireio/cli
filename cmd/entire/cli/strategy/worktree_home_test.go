package strategy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// A linked worktree session's home is the worktree holding its marker: one
// `git worktree move` took elsewhere is still it, and one git added at its
// path after removing it is not. Uses t.Chdir and t.Setenv — do NOT add
// t.Parallel().

// linkedHomeState saves sessionID homed in the linked worktree at linkedDir,
// with its marker, and returns it.
func linkedHomeState(t *testing.T, s *ManualCommitStrategy, sessionID, linkedDir string) *SessionState {
	t.Helper()
	state := &SessionState{
		SessionID:           sessionID,
		WorktreePath:        linkedDir,
		WorktreeIncarnation: mark(t, linkedDir),
		FilesTouched:        []string{"old.go"},
		TouchedFileHashes:   map[string]string{"old.go": "h-old"},
	}
	saveSessionMatchState(context.Background(), t, s, linkedDir, state)
	return mustLoad(t, s, sessionID)
}

// moveLinked moves the linked worktree to a new path and returns it.
func moveLinked(t *testing.T, mainDir, linkedDir string) string {
	t.Helper()
	moved := filepath.Join(resolvedTempDir(t), "moved")
	testutil.RunGit(t, mainDir, "worktree", "move", linkedDir, moved)
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, moved) })
	return moved
}

func TestHomeAt(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	ctx := context.Background()
	state := linkedHomeState(t, s, "home-at", linkedDir)

	assert.Equal(t, homeHere, homeAt(ctx, state, linkedDir))
	assert.Equal(t, homeUnknown, homeAt(ctx, state, mainDir), "the marker says nothing about a worktree with another ID")
	legacy := *state
	legacy.WorktreeIncarnation = ""
	assert.Equal(t, homeUnknown, homeAt(ctx, &legacy, linkedDir), "a session homed before markers has only its path")

	markerFile := filepath.Join(adminDir(t, mainDir, linkedDir), worktreeIncarnationFile)
	original, err := os.ReadFile(markerFile)
	require.NoError(t, err)

	// A marker Entire didn't write isn't the home's, but isn't known to be
	// another worktree's either.
	require.NoError(t, os.WriteFile(markerFile, []byte("not a marker"), 0o600))
	assert.Equal(t, homeNotHere, homeAt(ctx, state, linkedDir))

	// A check that fails says nothing: callers keep their path rule, so the
	// home still links its commits.
	require.NoError(t, os.Remove(markerFile))
	require.NoError(t, os.Mkdir(markerFile, 0o700))
	assert.Equal(t, homeUnknown, homeAt(ctx, state, linkedDir))
	states := []*SessionState{mustLoad(t, s, "home-at")}
	assert.Len(t, exactWorktreeMatches(ctx, states, linkedDir), 1, "a failed marker check stopped the home linking")

	require.NoError(t, os.Remove(markerFile))
	require.NoError(t, os.WriteFile(markerFile, original, 0o600))
	assert.Equal(t, homeHere, homeAt(ctx, state, linkedDir))
}

func TestInitializeSession_MarksALinkedHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}

	chdirWorktree(t, linkedDir)
	require.NoError(t, s.InitializeSession(t.Context(), "init-linked", agent.AgentTypeClaudeCode, "", "prompt", ""))
	assert.Equal(t, mark(t, linkedDir), mustLoad(t, s, "init-linked").WorktreeIncarnation)

	chdirWorktree(t, mainDir)
	require.NoError(t, s.InitializeSession(t.Context(), "init-main", agent.AgentTypeClaudeCode, "", "prompt", ""))
	assert.Empty(t, mustLoad(t, s, "init-main").WorktreeIncarnation, "the main worktree has no marker")
}

// A session homed before markers were kept gains one at its next turn at
// home, and with it move support.
func TestTurnStart_MarksALegacyLinkedHome(t *testing.T) {
	_, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	state := linkedHomeState(t, s, "legacy-home", linkedDir)
	state.WorktreeIncarnation = ""
	require.NoError(t, s.saveSessionState(t.Context(), state))

	chdirWorktree(t, linkedDir)
	require.NoError(t, s.InitializeSession(t.Context(), "legacy-home", agent.AgentTypeClaudeCode, "", "prompt", ""))
	assert.Equal(t, mark(t, linkedDir), mustLoad(t, s, "legacy-home").WorktreeIncarnation)
}

// G1: a turn starting in the home after `git worktree move` follows it.
func TestReconcileWorktreePathForResumedTurn_FollowsAMovedLinkedHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	state := linkedHomeState(t, s, "moved-home", linkedDir)
	worktreeID := state.WorktreeID
	moved := moveLinked(t, mainDir, linkedDir)

	// A copy of the moved worktree carries its .git file and so its marker,
	// but git's record of the worktree names the original: not the home.
	copied := filepath.Join(resolvedTempDir(t), "copied")
	require.NoError(t, os.CopyFS(copied, os.DirFS(moved)))
	chdirWorktree(t, copied)
	reconcileWorktreePathForResumedTurn(t.Context(), state)
	assert.Equal(t, linkedDir, state.WorktreePath, "a copy of the home was taken for it")

	chdirWorktree(t, moved)
	reconcileWorktreePathForResumedTurn(t.Context(), state)
	assert.Equal(t, moved, state.WorktreePath)
	assert.Equal(t, worktreeID, state.WorktreeID)
}

// G1: commit linking from the moved home finds the session as its own, in
// memory only; the stored state moves when a hook holds its lock.
func TestFindSessionsForWorktree_FindsAMovedHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	linkedHomeState(t, s, "moved-link", linkedDir)
	moved := moveLinked(t, mainDir, linkedDir)

	chdirWorktree(t, moved)
	states := mustListStates(t.Context(), t, s)
	matches, ambiguous := s.findSessionsForWorktreeFromStates(t.Context(), states, moved)
	require.False(t, ambiguous)
	require.Len(t, matches, 1, "the moved home's commit found no session")
	assert.Equal(t, moved, matches[0].WorktreePath, "the moved home isn't seen as the session's home")
	assert.Equal(t, linkedDir, mustLoad(t, s, "moved-link").WorktreePath, "linking stored the move without the session's lock")
}

// G1: a commit in the moved home condenses as the home's and stores its new
// path.
func TestPostCommit_StoresAMovedHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := NewManualCommitStrategy()
	chdirWorktree(t, linkedDir)
	setupSessionWithCheckpointAndFile(t, s, linkedDir, "moved-commit", "agent.go")
	require.NoError(t, MutateSessionState(t.Context(), "moved-commit", func(state *SessionState) error {
		state.WorktreeIncarnation = mark(t, linkedDir)
		return nil
	}))
	moved := moveLinked(t, mainDir, linkedDir)

	chdirWorktree(t, moved)
	testutil.GitAdd(t, moved, "agent.go")
	testutil.GitCommit(t, moved, "agent work\n\n"+trailers.CheckpointTrailerKey+": a1b2c3d4e5f6\n")
	require.NoError(t, NewManualCommitStrategy().PostCommit(t.Context()))

	state := mustLoad(t, s, "moved-commit")
	assert.Equal(t, moved, state.WorktreePath)
	assert.Empty(t, state.FilesTouched, "the commit in the moved home didn't consume its files")
	assert.Equal(t, testutil.GetHeadHash(t, moved), state.BaseCommit)
}

// G2: a worktree git added at the home's path after removing it is not the
// home: its commits don't link the old session, by path or as a sibling.
func TestFindSessionsForCommitLinking_ReaddedHomeIsNotTheHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	linkedHomeState(t, s, "readded-link", linkedDir)
	readdLinked(t, mainDir, linkedDir)

	chdirWorktree(t, linkedDir)
	sessions, err := s.findSessionsForCommitLinking(t.Context(), linkedDir)
	require.NoError(t, err)
	assert.Empty(t, sessions, "the removed home's session linked the new worktree's commit")
	exact, err := s.findExactSessionsForWorktree(t.Context(), linkedDir)
	require.NoError(t, err)
	assert.Empty(t, exact, "the new worktree's HEAD would move the removed home's base")
}

// G2: the session's own turn starting in the re-added worktree (the agent
// resumed there) makes it the home, dropping what the removed one held.
func TestTurnStart_RehomesInAReaddedHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	transcript := filepath.Join(t.TempDir(), "rehome.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(strings.Repeat(`{"type":"user"}`+"\n", 5)), 0o600))
	launched := time.Now().Add(-time.Hour)
	state := linkedHomeState(t, s, "rehome", linkedDir)
	state.AgentType = agent.AgentTypeClaudeCode
	state.TranscriptPath = transcript
	state.StepCount = 3
	state.CheckpointTranscriptStart = 1
	state.LastCheckpointID = id.MustCheckpointID("abc123def456")
	state.TokenUsage = &agent.TokenUsage{InputTokens: 42}
	state.TaskRecords = []session.TaskRecord{{ToolUseID: "bg-old", StartedAt: launched, Background: true}}
	oldBase := state.BaseCommit
	require.NoError(t, s.saveSessionState(t.Context(), state))
	readdLinked(t, mainDir, linkedDir)
	testutil.WriteFile(t, linkedDir, "new.txt", "new")
	testutil.GitAdd(t, linkedDir, "new.txt")
	testutil.GitCommit(t, linkedDir, "new worktree commit")
	require.NotEqual(t, oldBase, testutil.GetHeadHash(t, linkedDir))

	chdirWorktree(t, linkedDir)
	require.NoError(t, s.InitializeSession(t.Context(), "rehome", agent.AgentTypeClaudeCode, transcript, "prompt", ""))

	after := mustLoad(t, s, "rehome")
	assert.Empty(t, after.FilesTouched)
	assert.Empty(t, after.TouchedFileHashes)
	assert.Equal(t, testutil.GetHeadHash(t, linkedDir), after.BaseCommit)
	assert.Equal(t, mark(t, linkedDir), after.WorktreeIncarnation)
	assert.True(t, after.LastCheckpointID.IsEmpty())
	assert.Equal(t, 5, after.CheckpointTranscriptStart, "the removed worktree's turns would condense into the new one's next commit")
	assert.Zero(t, after.StepCount)
	require.NotNil(t, after.RehomedAt)
	assert.True(t, after.RehomedAt.After(launched))
	require.NotNil(t, after.TokenUsage)
	assert.Equal(t, 42, after.TokenUsage.InputTokens, "tokens are cumulative")
	assert.NotNil(t, after.FindTaskRecord("bg-old"), "a background task from before finishes on its own")
}

// G2: background tasks launched before a re-home ran in the removed
// worktree: they claim nothing in the new home, and don't stop its tasks
// claiming.
func TestLoadBackgroundClaimContext_IgnoresTasksFromBeforeARehome(t *testing.T) {
	_, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	state := linkedHomeState(t, s, "rehomed-claims", linkedDir)
	rehomed := time.Now().Add(-time.Minute)
	state.RehomedAt = &rehomed
	state.TaskRecords = []session.TaskRecord{
		{ToolUseID: "before", StartedAt: rehomed.Add(-time.Hour), Background: true},
		{ToolUseID: "after", StartedAt: rehomed.Add(time.Second), Background: true},
	}
	require.NoError(t, s.saveSessionState(t.Context(), state))

	claims, ok, err := LoadBackgroundClaimContext(t.Context(), "rehomed-claims", "after")
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, claims.Overlapped, "a task from the removed worktree overlapped the new home's")

	_, ok, err = LoadBackgroundClaimContext(t.Context(), "rehomed-claims", "before")
	require.NoError(t, err)
	assert.False(t, ok, "a task from the removed worktree claimed files in the new home")
}

// G2: a commit the session's agent makes in a worktree re-added at its old
// home's path is a guest's: the removed home's base and files stay as they
// were.
func TestPostCommit_ReaddedHomeCommitIsAGuests(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := NewManualCommitStrategy()
	chdirWorktree(t, linkedDir)
	setupSessionWithCheckpointAndFile(t, s, linkedDir, "readded-guest", "agent.go")
	require.NoError(t, MutateSessionState(t.Context(), "readded-guest", func(state *SessionState) error {
		state.WorktreeIncarnation = mark(t, linkedDir)
		state.Owner = selfAncestorOwner(t)
		state.Phase = session.PhaseActive
		return nil
	}))
	before := mustLoad(t, s, "readded-guest")
	require.NotEmpty(t, before.TouchedFileHashes)
	readdLinked(t, mainDir, linkedDir)

	chdirWorktree(t, linkedDir)
	writeStoredCopy(t, linkedDir, "readded-guest", "readded")
	testutil.WriteFile(t, linkedDir, "agent.go", "agent content for agent.go")
	testutil.GitAdd(t, linkedDir, "agent.go")
	testutil.GitCommit(t, linkedDir, "agent work\n\n"+trailers.CheckpointTrailerKey+": a1b2c3d4e5f6\n")
	require.NoError(t, NewManualCommitStrategy().PostCommit(t.Context()))

	after := mustLoad(t, s, "readded-guest")
	assert.Equal(t, before.BaseCommit, after.BaseCommit, "the new worktree's HEAD moved the removed home's base")
	assert.Equal(t, before.FilesTouched, after.FilesTouched, "the new worktree's commit consumed the removed home's files")
	assert.Equal(t, before.TouchedFileHashes, after.TouchedFileHashes)
}

// G3: a session that ended with a record of a worktree removed since has
// nothing left to condense.
func TestCondenseSessionByID_PrunesRemovedWorktrees(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	state := &SessionState{
		SessionID: "ended-removed",
		OtherWorktrees: map[string]*session.WorktreeWork{
			linkedKey(t, linkedDir): {Path: linkedDir, FilesTouched: []string{"gone.go"}, Incarnation: mark(t, linkedDir)},
		},
	}
	saveSessionMatchState(context.Background(), t, s, mainDir, state)
	require.NoError(t, MutateSessionState(t.Context(), "ended-removed", func(state *SessionState) error {
		state.Phase = session.PhaseEnded
		return nil
	}))
	testutil.RunGit(t, mainDir, "worktree", "remove", "--force", linkedDir)

	require.NoError(t, s.CondenseSessionByID(t.Context(), "ended-removed"))
	after, err := s.loadSessionState(t.Context(), "ended-removed")
	require.NoError(t, err)
	assert.Nil(t, after, "a removed worktree's files were condensed: %+v", after)
}

// A commit in another worktree of the repository still finds a marked
// linked-home session through the sibling fallback, as an unmarked one does.
func TestFindSessionsForWorktree_SiblingFallbackKeepsAMarkedHome(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	linkedHomeState(t, s, "marked-sibling", linkedDir)

	chdirWorktree(t, mainDir)
	clearSessionMatchCaches()
	sessions, err := s.findSessionsForWorktree(context.Background(), mainDir)
	require.NoError(t, err)
	require.Len(t, sessions, 1, "the marker dropped the session from the sibling fallback")
	assert.Equal(t, "marked-sibling", sessions[0].SessionID)
}

// A home moved away is still followed after another worktree is added at its
// old path: the worktree registered there has another ID.
func TestHomeAt_MovedHomeWhoseOldPathWasReused(t *testing.T) {
	mainDir, linkedDir := twoWorktrees(t)
	s := &ManualCommitStrategy{}
	ctx := context.Background()
	state := linkedHomeState(t, s, "moved-reused", linkedDir)
	moved := moveLinked(t, mainDir, linkedDir)
	testutil.RunGit(t, mainDir, "worktree", "add", linkedDir, "-b", "reused")
	require.NotEqual(t, linkedKey(t, moved), linkedKey(t, linkedDir), "git gave the new worktree the moved one's ID")

	assert.Equal(t, homeMovedHere, homeAt(ctx, state, moved), "a reused old path orphaned the moved home")
	assert.Equal(t, homeUnknown, homeAt(ctx, state, linkedDir), "the worktree at the old path has another ID")
}
