package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestReconcileWorktreePathForResumedTurn_RepointsAfterRelocation covers the
// #1890 relocation case: the recorded worktree path no longer resolves (the repo
// was renamed/moved), so a resumed turn must repoint WorktreePath at the current
// worktree, which shares the same git common dir as the session store.
func TestReconcileWorktreePathForResumedTurn_RepointsAfterRelocation(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	gonePath := resolvedRemovedTempDir(t)

	state := &SessionState{SessionID: "relocated-session", WorktreePath: gonePath}

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, mainDir, state.WorktreePath,
		"a stale recorded path should be repointed at the current worktree")
}

// TestReconcileWorktreePathForResumedTurn_LeavesValidSiblingUntouched proves the
// guard: when the recorded path is still a live worktree of this same repo (a
// concurrent sibling), reconciliation must not steal it, even though the current
// worktree differs.
func TestReconcileWorktreePathForResumedTurn_LeavesValidSiblingUntouched(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	sibling := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, sibling, "sibling")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, sibling) })

	state := &SessionState{SessionID: "sibling-session", WorktreePath: sibling}

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, sibling, state.WorktreePath,
		"a recorded path that still resolves to this repo must be left untouched")
}

// TestReconcileWorktreePathForResumedTurn_LeavesLinkedWorktreeSessionUntouched
// covers the disalignment guard: a session started in a linked worktree
// (WorktreeID != "") whose path is later gone must NOT be reconciled. Repointing
// its WorktreePath to the main worktree while keeping WorktreeID would leave the
// two describing different worktrees, which breaks the shadow-branch derivation
// in `entire clean`/`entire explain` and the post-commit base/attribution
// updates. Linked-worktree relocation is a documented non-goal.
func TestReconcileWorktreePathForResumedTurn_LeavesLinkedWorktreeSessionUntouched(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	linkedDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, linkedDir, "feature")

	linkedID, err := paths.GetWorktreeID(linkedDir)
	require.NoError(t, err)
	require.NotEmpty(t, linkedID, "linked worktree must have a non-empty WorktreeID")

	state := &SessionState{
		SessionID:    "relocated-linked-session",
		WorktreePath: linkedDir,
		WorktreeID:   linkedID,
	}

	// Remove the linked worktree so its recorded path no longer resolves — the
	// condition that would trigger reconciliation for a main-worktree session.
	removeSessionMatchWorktree(mainDir, linkedDir)

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, linkedDir, state.WorktreePath,
		"a linked-worktree session must be left untouched (no path/ID disalignment)")
	require.Equal(t, linkedID, state.WorktreeID,
		"a linked-worktree session's WorktreeID must be left untouched")
}

// TestReconcileWorktreePathForResumedTurn_LeavesSessionResumedFromLinkedWorktree
// covers the reverse disalignment guard: a main-worktree session (WorktreeID "")
// resumed from a LINKED worktree must NOT be reconciled, otherwise WorktreePath
// would point at the linked worktree while WorktreeID stayed "".
func TestReconcileWorktreePathForResumedTurn_LeavesSessionResumedFromLinkedWorktree(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	linkedDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, linkedDir, "feature")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, linkedDir) })

	gonePath := resolvedRemovedTempDir(t)
	state := &SessionState{SessionID: "main-session", WorktreePath: gonePath}

	// Resume from the linked worktree rather than the main worktree.
	t.Chdir(linkedDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, gonePath, state.WorktreePath,
		"resuming into a linked worktree must not reconcile (would disalign path/ID)")
}

// TestReconcileWorktreePathForResumedTurn_NoopWhenPathUnchanged covers the common
// path: a normal turn from the recorded worktree leaves the state alone.
func TestReconcileWorktreePathForResumedTurn_NoopWhenPathUnchanged(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)

	state := &SessionState{SessionID: "steady-session", WorktreePath: mainDir}

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, mainDir, state.WorktreePath)
}
