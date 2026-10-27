package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// A linked worktree session's home is known by its marker. These tests use
// t.Chdir — do NOT add t.Parallel().

// addMarkedWorktree adds a linked worktree of repo at a resolved path and
// returns it with its git ID and marker.
func addMarkedWorktree(t *testing.T, repo, name string) (dir, worktreeID, marker string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), name)
	testutil.RunGit(t, repo, "worktree", "add", dir, "-b", name)
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	worktreeID, err = paths.GetWorktreeID(resolved)
	require.NoError(t, err)
	marker, err = strategy.MarkWorktree(context.Background(), resolved, worktreeID)
	require.NoError(t, err)
	return resolved, worktreeID, marker
}

func chdirAdoptTarget(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
}

// Adopting into a linked worktree homes the session by that worktree's
// marker; the main worktree has none.
func TestBuildAdoptedSessionState_MarksTheTargetHome(t *testing.T) {
	repo := setupAdoptRepo(t)
	target, _, targetMarker := addMarkedWorktree(t, repo, "target")
	source := &session.State{SessionID: "adopt-mark", WorktreePath: repo, BaseCommit: testutil.GetHeadHash(t, repo), Phase: session.PhaseActive, AgentType: agent.AgentTypeClaudeCode}

	chdirAdoptTarget(t, target)
	adopted, _, err := buildAdoptedSessionState(t.Context(), source, adoptAgentHome{}, repo)
	require.NoError(t, err)
	assert.Equal(t, targetMarker, adopted.WorktreeIncarnation)

	linkedSource := *adopted
	chdirAdoptTarget(t, repo)
	adopted, _, err = buildAdoptedSessionState(t.Context(), &linkedSource, adoptAgentHome{}, target)
	require.NoError(t, err)
	assert.Empty(t, adopted.WorktreeIncarnation, "the main worktree has no marker")
}

// A source home git removed and re-added at the same path is another
// worktree: adopting carries none of the removed one's pending files.
func TestBuildAdoptedSessionState_DropsAReaddedSourceHomesFiles(t *testing.T) {
	repo := setupAdoptRepo(t)
	src, srcID, srcMarker := addMarkedWorktree(t, repo, "src")
	source := &session.State{
		SessionID:           "adopt-readded",
		WorktreePath:        src,
		WorktreeID:          srcID,
		WorktreeIncarnation: srcMarker,
		BaseCommit:          testutil.GetHeadHash(t, repo),
		FilesTouched:        []string{"src.go"},
		Phase:               session.PhaseActive,
		AgentType:           agent.AgentTypeClaudeCode,
	}
	testutil.RunGit(t, repo, "worktree", "remove", "--force", src)
	testutil.RunGit(t, repo, "worktree", "add", src, "-b", "src-readded")

	chdirAdoptTarget(t, repo)
	adopted, _, err := buildAdoptedSessionState(t.Context(), source, adoptAgentHome{}, src)
	require.NoError(t, err)
	assert.Nil(t, adopted.OtherWorktrees[session.WorktreeKey(srcID)], "the removed home's files were carried into the new worktree's record")
}

// A session that ended with a record of a worktree removed since its last
// turn end has it pruned at session end, so it doesn't stay pending.
func TestHandleLifecycleSessionEnd_PrunesRemovedWorktrees(t *testing.T) {
	setupStopTestRepo(t)
	ctx := t.Context()
	root, err := paths.WorktreeRoot(ctx)
	require.NoError(t, err)
	linked, linkedID, marker := addMarkedWorktree(t, root, "linked")
	require.NoError(t, strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:    "end-prune",
		StartedAt:    time.Now(),
		Phase:        session.PhaseIdle,
		WorktreePath: root,
		BaseCommit:   testutil.GetHeadHash(t, root),
		OtherWorktrees: map[string]*session.WorktreeWork{
			session.WorktreeKey(linkedID): {Path: linked, FilesTouched: []string{"gone.go"}, Incarnation: marker},
		},
	}))
	testutil.RunGit(t, root, "worktree", "remove", "--force", linked)

	require.NoError(t, handleLifecycleSessionEnd(ctx, newMockAgent(), &agent.Event{SessionID: "end-prune"}))
	state, err := strategy.LoadSessionState(ctx, "end-prune")
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Empty(t, state.OtherWorktrees, "the ended session still holds a removed worktree's files")
}
