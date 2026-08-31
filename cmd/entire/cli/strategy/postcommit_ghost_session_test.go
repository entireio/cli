package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostCommit_GhostActiveSessionDoesNotPinShadowBranch reproduces the agy
// headless-subagent failure from live E2E (TestSubagentCommitFlow): agy runs a
// subagent as a SEPARATE conversation with its own hooks. The subagent's Stop
// arrives fullyIdle=true (session condenses normally at commit), but the
// parent conversation only ever sends fullyIdle=false Stops before the process
// exits — leaving a ghost session that is ACTIVE forever with no tracked files
// and no checkpoints. The shadow-branch cleanup preserved the branch for ANY
// active session, so the ghost pinned it indefinitely.
//
// An active session with nothing uncondensed to lose (no FilesTouched) must
// not pin the branch: SaveStep recreates shadow branches on demand, so nothing
// is lost if it does produce work later.
func TestPostCommit_GhostActiveSessionDoesNotPinShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	workerID := "test-ghost-worker" // the subagent conversation: did the work
	ghostID := "test-ghost-parent"  // the parent conversation: ghost-ACTIVE

	// Worker: checkpoint on the shadow branch, went IDLE at its Stop.
	setupSessionWithCheckpoint(t, s, repo, dir, workerID)
	worker, err := s.loadSessionState(context.Background(), workerID)
	require.NoError(t, err)
	now := time.Now()
	worker.Phase = session.PhaseIdle
	worker.LastInteractionTime = &now
	require.NoError(t, s.saveSessionState(context.Background(), worker))
	shadowBranch := getShadowBranchNameForCommit(worker.BaseCommit, worker.WorktreeID)

	// Ghost parent: ACTIVE, recent, same base commit, NO files, NO checkpoints.
	ghost := &SessionState{
		SessionID:           ghostID,
		AgentType:           worker.AgentType,
		BaseCommit:          worker.BaseCommit,
		WorktreeID:          worker.WorktreeID,
		WorktreePath:        worker.WorktreePath, // PostCommit filters sessions by worktree path
		Phase:               session.PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
	}
	require.NoError(t, s.saveSessionState(context.Background(), ghost))

	// User commits the worker's file; PostCommit condenses the worker.
	commitWithCheckpointTrailer(t, repo, dir, "aabb00112233")
	require.NoError(t, s.PostCommit(context.Background()))

	// The worker condensed to the metadata branch...
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "worker session should have condensed")

	// ...and the ghost must NOT pin the shadow branch.
	_, err = repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	assert.Error(t, err,
		"shadow branch must be deleted: the only active session has no tracked files and no uncondensed content to lose")
}

// TestPostCommit_ActiveSessionWithFilesStillPinsShadowBranch is the inverse
// guard: an ACTIVE session that DOES have uncommitted tracked files must keep
// pinning the shadow branch (its uncondensed checkpoints are still needed).
func TestPostCommit_ActiveSessionWithFilesStillPinsShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	workerID := "test-pin-worker"
	activeID := "test-pin-active"

	setupSessionWithCheckpoint(t, s, repo, dir, workerID)
	worker, err := s.loadSessionState(context.Background(), workerID)
	require.NoError(t, err)
	now := time.Now()
	worker.Phase = session.PhaseIdle
	worker.LastInteractionTime = &now
	require.NoError(t, s.saveSessionState(context.Background(), worker))
	shadowBranch := getShadowBranchNameForCommit(worker.BaseCommit, worker.WorktreeID)

	// A genuinely mid-turn session with its own uncommitted tracked file.
	active := &SessionState{
		SessionID:           activeID,
		AgentType:           worker.AgentType,
		BaseCommit:          worker.BaseCommit,
		WorktreeID:          worker.WorktreeID,
		WorktreePath:        worker.WorktreePath, // PostCommit filters sessions by worktree path
		Phase:               session.PhaseActive,
		StartedAt:           now,
		LastInteractionTime: &now,
		FilesTouched:        []string{"other-uncommitted.txt"},
	}
	require.NoError(t, s.saveSessionState(context.Background(), active))

	commitWithCheckpointTrailer(t, repo, dir, "ccdd00112233")
	require.NoError(t, s.PostCommit(context.Background()))

	_, err = repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
	assert.NoError(t, err,
		"shadow branch must be preserved for an active session with uncommitted tracked files")
}
