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

// TestPostCommit_ActiveSessionWithShadowContentStillPinsShadowBranch is the
// inverse guard: an ACTIVE session that still has content on the shadow branch
// must keep pinning it (its uncondensed checkpoints are still needed). Files
// are not the only such content — StepCount only counts checkpoint commits
// that were actually written and is reset on condensation, so a session whose
// first checkpoint captured a transcript before any edit (a read-only tool
// call, a question answered without touching the tree) has exactly one commit
// on the branch and nothing else to prove it by.
func TestPostCommit_ActiveSessionWithShadowContentStillPinsShadowBranch(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(active *SessionState)
	}{
		{name: "uncommitted tracked file", mutate: func(a *SessionState) {
			a.FilesTouched = []string{"other-uncommitted.txt"}
		}},
		{name: "written step with no files yet", mutate: func(a *SessionState) {
			a.StepCount = 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Not parallel: t.Chdir is process-global.
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

			// A genuinely mid-turn session with its own uncondensed content.
			active := &SessionState{
				SessionID:           activeID,
				AgentType:           worker.AgentType,
				BaseCommit:          worker.BaseCommit,
				WorktreeID:          worker.WorktreeID,
				WorktreePath:        worker.WorktreePath, // PostCommit filters sessions by worktree path
				Phase:               session.PhaseActive,
				StartedAt:           now,
				LastInteractionTime: &now,
			}
			tc.mutate(active)
			require.NoError(t, s.saveSessionState(context.Background(), active))

			commitWithCheckpointTrailer(t, repo, dir, "ccdd00112233")
			require.NoError(t, s.PostCommit(context.Background()))

			_, err = repo.Reference(plumbing.NewBranchReferenceName(shadowBranch), true)
			assert.NoError(t, err,
				"shadow branch must be preserved for an active session with uncondensed shadow content")
		})
	}
}
