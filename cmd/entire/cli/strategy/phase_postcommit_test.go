package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testNewActiveSessionID = "new-active-session"

// TestPostCommit_ActiveSession_CondensesImmediately verifies that PostCommit on
// an ACTIVE session condenses immediately and stays ACTIVE.
// With the 1:1 checkpoint model, each commit gets its own checkpoint right away.
func TestPostCommit_ActiveSession_CondensesImmediately(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-active"

	// Initialize session and save a checkpoint so there is shadow branch content
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE (simulating agent mid-turn)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	// Create a commit WITH the Entire-Checkpoint trailer on the main branch
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify phase stays ACTIVE (immediate condensation, no deferred phase)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"ACTIVE session should stay ACTIVE after immediate condensation on GitCommit")

	// Verify condensation happened: the entire/checkpoints/v1 branch should exist
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after immediate condensation")
	assert.NotNil(t, sessionsRef)

	// Verify StepCount was reset to 0 by condensation
	assert.Equal(t, 0, state.StepCount,
		"StepCount should be reset after immediate condensation")
}

// TestPostCommit_IdleSession_Condenses verifies that PostCommit on an IDLE
// session condenses session data and cleans up the shadow branch.
func TestPostCommit_IdleSession_Condenses(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-idle"

	// Initialize session and save a checkpoint so there is shadow branch content
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to IDLE (agent turn finished, waiting for user)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.LastInteractionTime = nil
	state.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(state))

	// Record shadow branch name before PostCommit
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

	// Create a commit WITH the Entire-Checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "b2c3d4e5f6a1")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify condensation happened: the entire/checkpoints/v1 branch should exist
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after condensation")
	assert.NotNil(t, sessionsRef)

	// Verify shadow branch IS deleted after condensation
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	assert.Error(t, err,
		"shadow branch should be deleted after condensation for IDLE session")
}

// TestPostCommit_RebaseDuringActive_SkipsTransition verifies that PostCommit
// is a no-op during rebase operations, leaving the session phase unchanged.
func TestPostCommit_RebaseDuringActive_SkipsTransition(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-rebase"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	// Capture shadow branch name BEFORE any state changes
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	originalStepCount := state.StepCount

	// Simulate rebase in progress by creating .git/rebase-merge/ directory
	gitDir := filepath.Join(dir, ".git")
	rebaseMergeDir := filepath.Join(gitDir, "rebase-merge")
	require.NoError(t, os.MkdirAll(rebaseMergeDir, 0o755))
	defer os.RemoveAll(rebaseMergeDir)

	// Create a commit WITH the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "c3d4e5f6a1b2")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify phase stayed ACTIVE (no transition during rebase)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, session.PhaseActive, state.Phase,
		"session should stay ACTIVE during rebase (no transition)")

	// Verify StepCount was NOT reset (no condensation happened)
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged - no condensation during rebase")

	// Verify NO condensation happened (entire/checkpoints/v1 branch should not exist)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist - no condensation during rebase")

	// Verify shadow branch still exists (not cleaned up during rebase)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	assert.NoError(t, err,
		"shadow branch should be preserved during rebase")
}

// TestPostCommit_ActiveSessionAlwaysCondenses verifies that an ACTIVE session
// is always condensed on GitCommit, even when it has no checkpoints or tracked files.
// This is because PrepareCommitMsg already validated the trailer, so PostCommit
// trusts that decision rather than re-validating via transcript analysis (which is
// unreliable when subagents are still running).
func TestPostCommit_ActiveSessionAlwaysCondenses(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	idleSessionID := "test-postcommit-idle-multi"
	activeSessionID := "test-postcommit-active-multi"

	// Initialize the idle session with a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, idleSessionID)

	// Get worktree path and base commit from the idle session
	idleState, err := s.loadSessionState(idleSessionID)
	require.NoError(t, err)
	worktreePath := idleState.WorktreePath
	baseCommit := idleState.BaseCommit
	worktreeID := idleState.WorktreeID

	// Set idle session to IDLE phase
	idleState.Phase = session.PhaseIdle
	idleState.LastInteractionTime = nil
	idleState.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(idleState))

	// Create a second session with the SAME base commit and worktree (concurrent session).
	// This session is ACTIVE but has NO checkpoints (StepCount=0, no shadow branch content).
	// Despite having no content, it WILL be condensed because ACTIVE sessions always
	// condense — PrepareCommitMsg already validated the trailer.
	now := time.Now()
	activeState := &SessionState{
		SessionID:           activeSessionID,
		BaseCommit:          baseCommit,
		WorktreePath:        worktreePath,
		WorktreeID:          worktreeID,
		StartedAt:           now,
		Phase:               session.PhaseActive,
		LastInteractionTime: &now,
		StepCount:           0,
	}
	require.NoError(t, s.saveSessionState(activeState))

	// Record shadow branch name before PostCommit
	shadowBranch := getShadowBranchNameForCommit(baseCommit, worktreeID)

	// Create a commit WITH the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "d4e5f6a1b2c3")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify the ACTIVE session stays ACTIVE (immediate condensation model)
	activeState, err = s.loadSessionState(activeSessionID)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, activeState.Phase,
		"ACTIVE session should stay ACTIVE after GitCommit")

	// Verify both sessions condensed (entire/checkpoints/v1 branch should exist)
	idleState, err = s.loadSessionState(idleSessionID)
	require.NoError(t, err)
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after condensation")
	require.NotNil(t, sessionsRef)

	// Verify IDLE session's StepCount was reset by condensation
	assert.Equal(t, 0, idleState.StepCount,
		"IDLE session StepCount should be reset after condensation")

	// Verify shadow branch is cleaned up because ALL sessions condensed
	// (both IDLE and ACTIVE were condensed on this commit)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	assert.Error(t, err,
		"shadow branch should be deleted when all sessions have been condensed")
}

// TestPostCommit_CondensationFailure_PreservesShadowBranch verifies that when
// condensation fails (corrupted shadow branch), BaseCommit is NOT updated.
func TestPostCommit_CondensationFailure_PreservesShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-condense-fail-idle"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to IDLE
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.LastInteractionTime = nil
	state.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(state))

	// Record original BaseCommit and StepCount before corruption
	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// Corrupt shadow branch by pointing it at ZeroHash
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	corruptRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), plumbing.ZeroHash)
	require.NoError(t, repo.Storer.SetReference(corruptRef))

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "e5f6a1b2c3d4")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err, "PostCommit should not return error even when condensation fails")

	// Verify BaseCommit was NOT updated (condensation failed)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should NOT be updated when condensation fails")
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should NOT be reset when condensation fails")

	// Verify entire/checkpoints/v1 branch does NOT exist (condensation failed)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist when condensation fails")

	// Phase transition still applies even when condensation fails
	assert.Equal(t, session.PhaseIdle, state.Phase,
		"phase should remain IDLE when condensation fails")
}

// TestPostCommit_IdleSession_NoNewContent_PreservesBaseCommit verifies that when
// an IDLE session has no new transcript content since last condensation,
// PostCommit skips condensation and does NOT update BaseCommit.
//
// This prevents the bug where old IDLE sessions would have their BaseCommit
// incorrectly updated, causing them to be condensed on future commits.
func TestPostCommit_IdleSession_NoNewContent_PreservesBaseCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-idle-no-content"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to IDLE with CheckpointTranscriptStart matching transcript length (2 lines)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.LastInteractionTime = nil
	state.CheckpointTranscriptStart = 2 // Transcript has exactly 2 lines
	require.NoError(t, s.saveSessionState(state))

	// Record shadow branch name and original BaseCommit
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "f6a1b2c3d4e5")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify BaseCommit was NOT updated (IDLE sessions don't get BaseCommit updated)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should NOT be updated for IDLE session with no new content")

	// Shadow branch should still exist (not deleted, no condensation)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	require.NoError(t, err,
		"shadow branch should still exist when no condensation happened")

	// entire/checkpoints/v1 branch should NOT exist (no condensation)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist when no condensation happened")

	// StepCount should be unchanged
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged when no condensation happened")
}

// TestPostCommit_EndedSession_FilesTouched_Condenses verifies that an ENDED
// session with files touched and new content condenses on commit.
func TestPostCommit_EndedSession_FilesTouched_Condenses(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-ended-condenses"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ENDED with files touched
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	now := time.Now()
	state.Phase = session.PhaseEnded
	state.EndedAt = &now
	state.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(state))

	// Record shadow branch name
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f7")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify entire/checkpoints/v1 branch exists (condensation happened)
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after condensation")
	assert.NotNil(t, sessionsRef)

	// Verify old shadow branch is deleted after condensation
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	require.Error(t, err,
		"shadow branch should be deleted after condensation for ENDED session")

	// Verify StepCount was reset by condensation
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, 0, state.StepCount,
		"StepCount should be reset after condensation")

	// Phase stays ENDED
	assert.Equal(t, session.PhaseEnded, state.Phase,
		"ENDED session should stay ENDED after condensation")
}

// TestPostCommit_EndedSession_FilesTouched_NoNewContent verifies that an ENDED
// session with files touched but no new transcript content skips condensation
// and does NOT update BaseCommit.
//
// This prevents the bug where old ENDED sessions would have their BaseCommit
// incorrectly updated, causing them to be condensed on future commits.
func TestPostCommit_EndedSession_FilesTouched_NoNewContent(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-ended-no-content"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ENDED with files touched but no new content
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	now := time.Now()
	state.Phase = session.PhaseEnded
	state.EndedAt = &now
	state.FilesTouched = []string{"test.txt"}
	state.CheckpointTranscriptStart = 2 // Transcript has exactly 2 lines
	require.NoError(t, s.saveSessionState(state))

	// Record shadow branch name, original BaseCommit, and StepCount
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "b2c3d4e5f6a2")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify entire/checkpoints/v1 branch does NOT exist (no condensation)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist when no new content")

	// Shadow branch should still exist
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	require.NoError(t, err,
		"shadow branch should still exist when no condensation happened")

	// BaseCommit should NOT be updated (ENDED sessions don't get BaseCommit updated)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should NOT be updated for ENDED session with no new content")

	// StepCount unchanged
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged when no condensation happened")
}

// TestPostCommit_EndedSession_NoFilesTouched_Discards verifies that an ENDED
// session with no files touched takes the discard path and does NOT update BaseCommit.
//
// This prevents the bug where old ENDED sessions would have their BaseCommit
// incorrectly updated, causing them to be condensed on future commits.
func TestPostCommit_EndedSession_NoFilesTouched_Discards(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-ended-discard"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ENDED with no files touched
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	now := time.Now()
	state.Phase = session.PhaseEnded
	state.EndedAt = &now
	state.FilesTouched = nil // No files touched
	require.NoError(t, s.saveSessionState(state))

	// Record original BaseCommit and StepCount
	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "c3d4e5f6a1b3")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify entire/checkpoints/v1 branch does NOT exist (no condensation for discard path)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist for discard path")

	// BaseCommit should NOT be updated (ENDED sessions don't get BaseCommit updated)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should NOT be updated for ENDED session on discard path")

	// StepCount unchanged
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged on discard path")

	// Phase stays ENDED
	assert.Equal(t, session.PhaseEnded, state.Phase,
		"ENDED session should stay ENDED on discard path")
}

// TestPostCommit_CondensationFailure_EndedSession_PreservesShadowBranch verifies
// that when condensation fails for an ENDED session with files touched,
// BaseCommit is preserved (not updated).
func TestPostCommit_CondensationFailure_EndedSession_PreservesShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-condense-fail-ended"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ENDED with files touched
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	now := time.Now()
	state.Phase = session.PhaseEnded
	state.EndedAt = &now
	state.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(state))

	// Record original BaseCommit and StepCount
	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// Corrupt shadow branch by pointing it at ZeroHash
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	corruptRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), plumbing.ZeroHash)
	require.NoError(t, repo.Storer.SetReference(corruptRef))

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "e5f6a1b2c3d5")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err, "PostCommit should not return error even when condensation fails")

	// Verify BaseCommit was NOT updated (condensation failed)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should NOT be updated when condensation fails for ENDED session")
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should NOT be reset when condensation fails for ENDED session")

	// Verify entire/checkpoints/v1 branch does NOT exist (condensation failed)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist when condensation fails")

	// Phase stays ENDED
	assert.Equal(t, session.PhaseEnded, state.Phase,
		"ENDED session should stay ENDED when condensation fails")
}

// TestTurnEnd_Active_NoActions verifies that HandleTurnEnd with no actions
// is a no-op (normal ACTIVE → IDLE transition has no strategy-specific actions).
func TestTurnEnd_Active_NoActions(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turnend-no-actions"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE (normal turn, no commit during turn)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// ACTIVE + TurnEnd → IDLE with no strategy-specific actions
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})

	// Apply transition with no-op handler (no strategy actions for ACTIVE → IDLE)
	err = session.ApplyTransition(state, result, session.NoOpActionHandler{})
	require.NoError(t, err)

	// Call HandleTurnEnd — should be a no-op (no TurnCheckpointIDs)
	err = s.HandleTurnEnd(state)
	require.NoError(t, err)

	// Verify state is unchanged
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should be unchanged for no-op turn end")
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged for no-op turn end")

	// Shadow branch should still exist (not cleaned up)
	shadowBranch := getShadowBranchNameForCommit(originalBaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	assert.NoError(t, err,
		"shadow branch should still exist after no-op turn end")
}

// TestPostCommit_FilesTouched_ResetsAfterCondensation verifies that FilesTouched
// is reset after condensation, so subsequent condensations only contain the files
// touched since the last commit — not the accumulated history.
func TestPostCommit_FilesTouched_ResetsAfterCondensation(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-filestouched-reset"

	// --- Round 1: Save checkpoint touching files A.txt and B.txt ---

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"round 1 prompt"}}
{"type":"assistant","message":{"content":"round 1 response"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	// Create files A.txt and B.txt
	require.NoError(t, os.WriteFile(filepath.Join(dir, "A.txt"), []byte("file A"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "B.txt"), []byte("file B"), 0o644))

	err = s.SaveStep(StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"A.txt", "B.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1: files A and B",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Set phase to IDLE so PostCommit triggers immediate condensation
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	require.NoError(t, s.saveSessionState(state))

	// Verify FilesTouched has A.txt and B.txt before condensation
	assert.ElementsMatch(t, []string{"A.txt", "B.txt"}, state.FilesTouched,
		"FilesTouched should contain A.txt and B.txt before first condensation")

	// --- Commit A.txt, B.txt and condense (round 1) ---
	checkpointID1 := "a1a2a3a4a5a6"
	commitFilesWithTrailer(t, repo, dir, checkpointID1, "A.txt", "B.txt")

	err = s.PostCommit()
	require.NoError(t, err)

	// Verify condensation happened
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 should exist after first condensation")

	// Verify first condensation contains A.txt and B.txt
	store := checkpoint.NewGitStore(repo)
	cpID1 := id.MustCheckpointID(checkpointID1)
	summary1, err := store.ReadCommitted(context.Background(), cpID1)
	require.NoError(t, err)
	require.NotNil(t, summary1)
	assert.ElementsMatch(t, []string{"A.txt", "B.txt"}, summary1.FilesTouched,
		"First condensation should contain A.txt and B.txt")

	// Verify FilesTouched was reset after condensation
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.FilesTouched,
		"FilesTouched should be nil after condensation (all files were committed)")

	// --- Round 2: Save checkpoint touching files C.txt and D.txt ---

	// Append to transcript for round 2
	transcript2 := `{"type":"human","message":{"content":"round 2 prompt"}}
{"type":"assistant","message":{"content":"round 2 response"}}
`
	f, err := os.OpenFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(transcript2)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Create files C.txt and D.txt
	require.NoError(t, os.WriteFile(filepath.Join(dir, "C.txt"), []byte("file C"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "D.txt"), []byte("file D"), 0o644))

	err = s.SaveStep(StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"C.txt", "D.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2: files C and D",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Set phase to IDLE for immediate condensation
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	require.NoError(t, s.saveSessionState(state))

	// Verify FilesTouched only has C.txt and D.txt (NOT A.txt, B.txt)
	assert.ElementsMatch(t, []string{"C.txt", "D.txt"}, state.FilesTouched,
		"FilesTouched should only contain C.txt and D.txt after reset")

	// --- Commit C.txt, D.txt and condense (round 2) ---
	checkpointID2 := "b1b2b3b4b5b6"
	commitFilesWithTrailer(t, repo, dir, checkpointID2, "C.txt", "D.txt")

	err = s.PostCommit()
	require.NoError(t, err)

	// Verify second condensation contains ONLY C.txt and D.txt
	cpID2 := id.MustCheckpointID(checkpointID2)
	summary2, err := store.ReadCommitted(context.Background(), cpID2)
	require.NoError(t, err)
	require.NotNil(t, summary2, "Second condensation should exist")
	assert.ElementsMatch(t, []string{"C.txt", "D.txt"}, summary2.FilesTouched,
		"Second condensation should only contain C.txt and D.txt, not accumulated files from first condensation")
}

// TestSubtractFiles verifies that subtractFiles correctly removes files present
// in the exclude set and preserves files not in it.
func TestSubtractFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    []string
		exclude  map[string]struct{}
		expected []string
	}{
		{
			name:     "no overlap",
			files:    []string{"a.txt", "b.txt"},
			exclude:  map[string]struct{}{"c.txt": {}},
			expected: []string{"a.txt", "b.txt"},
		},
		{
			name:     "full overlap",
			files:    []string{"a.txt", "b.txt"},
			exclude:  map[string]struct{}{"a.txt": {}, "b.txt": {}},
			expected: nil,
		},
		{
			name:     "partial overlap",
			files:    []string{"a.txt", "b.txt", "c.txt"},
			exclude:  map[string]struct{}{"b.txt": {}},
			expected: []string{"a.txt", "c.txt"},
		},
		{
			name:     "empty files",
			files:    []string{},
			exclude:  map[string]struct{}{"a.txt": {}},
			expected: nil,
		},
		{
			name:     "empty exclude",
			files:    []string{"a.txt", "b.txt"},
			exclude:  map[string]struct{}{},
			expected: []string{"a.txt", "b.txt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := subtractFiles(tt.files, tt.exclude)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestFilesChangedInCommit verifies that filesChangedInCommit correctly extracts
// the set of files changed in a commit by diffing against its parent.
func TestFilesChangedInCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	// Create files and commit them
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("content1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("content2"), 0o644))
	_, err = wt.Add("file1.txt")
	require.NoError(t, err)
	_, err = wt.Add("file2.txt")
	require.NoError(t, err)

	commitHash, err := wt.Commit("add files", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	changed := filesChangedInCommit(commit)
	assert.Contains(t, changed, "file1.txt")
	assert.Contains(t, changed, "file2.txt")
	// test.txt was in the initial commit, not this one
	assert.NotContains(t, changed, "test.txt")
}

// TestFilesChangedInCommit_InitialCommit verifies that filesChangedInCommit
// handles the initial commit (no parent) by listing all files.
func TestFilesChangedInCommit_InitialCommit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	cfg, err := repo.Config()
	require.NoError(t, err)
	cfg.User.Name = "Test"
	cfg.User.Email = "test@test.com"
	require.NoError(t, repo.SetConfig(cfg))

	wt, err := repo.Worktree()
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "init.txt"), []byte("initial"), 0o644))
	_, err = wt.Add("init.txt")
	require.NoError(t, err)

	commitHash, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	commit, err := repo.CommitObject(commitHash)
	require.NoError(t, err)

	changed := filesChangedInCommit(commit)
	assert.Contains(t, changed, "init.txt")
	assert.Len(t, changed, 1)
}

// TestPostCommit_ActiveSession_CarryForward_PartialCommit verifies that when an
// ACTIVE session has touched files A, B, C but only A and B are committed, the
// remaining file C is carried forward to a new shadow branch.
func TestPostCommit_ActiveSession_CarryForward_PartialCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-carry-forward-partial"

	// Create metadata directory with transcript
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"create files A B C"}}
{"type":"assistant","message":{"content":"creating files"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	// Create all three files
	require.NoError(t, os.WriteFile(filepath.Join(dir, "A.txt"), []byte("file A"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "B.txt"), []byte("file B"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "C.txt"), []byte("file C"), 0o644))

	// Save checkpoint with all three files
	err = s.SaveStep(StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"A.txt", "B.txt", "C.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint: files A, B, C",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Set phase to ACTIVE (agent mid-turn)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	// Verify FilesTouched contains all three files
	assert.ElementsMatch(t, []string{"A.txt", "B.txt", "C.txt"}, state.FilesTouched)

	// Commit ONLY A.txt and B.txt (not C.txt) with checkpoint trailer
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("A.txt")
	require.NoError(t, err)
	_, err = wt.Add("B.txt")
	require.NoError(t, err)

	cpID := "cf1cf2cf3cf4"
	commitMsg := "commit A and B\n\n" + trailers.CheckpointTrailerKey + ": " + cpID + "\n"
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify session stayed ACTIVE
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase)

	// Verify carry-forward: FilesTouched should now only contain C.txt
	assert.Equal(t, []string{"C.txt"}, state.FilesTouched,
		"carry-forward should preserve only the uncommitted file C.txt")

	// Verify StepCount was set to 1 (carry-forward creates a new checkpoint)
	assert.Equal(t, 1, state.StepCount,
		"carry-forward should set StepCount to 1")

	// Verify CheckpointTranscriptStart was reset to 0 (prompt-level carry-forward)
	assert.Equal(t, 0, state.CheckpointTranscriptStart,
		"carry-forward should reset CheckpointTranscriptStart to 0 for full transcript reprocessing")

	// Verify LastCheckpointID was cleared (next commit generates fresh ID)
	assert.Empty(t, state.LastCheckpointID,
		"carry-forward should clear LastCheckpointID")

	// Verify a new shadow branch exists at the new HEAD
	newShadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(newShadowBranch), true)
	assert.NoError(t, err,
		"carry-forward should create a new shadow branch at the new HEAD")
}

// TestPostCommit_ActiveSession_CarryForward_AllCommitted verifies that when an
// ACTIVE session's files are ALL included in the commit, no carry-forward occurs.
func TestPostCommit_ActiveSession_CarryForward_AllCommitted(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-carry-forward-all"

	// Initialize session and save a checkpoint with files A and B
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"create files A B"}}
{"type":"assistant","message":{"content":"creating files"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "A.txt"), []byte("file A"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "B.txt"), []byte("file B"), 0o644))

	err = s.SaveStep(StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"A.txt", "B.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint: files A, B",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	// Set phase to ACTIVE
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	// Commit ALL files (A.txt and B.txt) with checkpoint trailer
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("A.txt")
	require.NoError(t, err)
	_, err = wt.Add("B.txt")
	require.NoError(t, err)

	cpID := "cf5cf6cf7cf8"
	commitMsg := "commit A and B\n\n" + trailers.CheckpointTrailerKey + ": " + cpID + "\n"
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify session stayed ACTIVE
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state.Phase)

	// Verify NO carry-forward: FilesTouched should be nil (all condensed, nothing remaining)
	assert.Nil(t, state.FilesTouched,
		"when all files are committed, no carry-forward should occur (FilesTouched cleared by condensation)")

	// Verify StepCount was reset to 0 by condensation (not 1 from carry-forward)
	assert.Equal(t, 0, state.StepCount,
		"without carry-forward, StepCount should be reset to 0 by condensation")
}

// TestPostCommit_ActiveSession_RecordsTurnCheckpointIDs verifies that PostCommit
// records the checkpoint ID in TurnCheckpointIDs for ACTIVE sessions.
// This enables HandleTurnEnd to finalize all checkpoints with the full transcript.
func TestPostCommit_ActiveSession_RecordsTurnCheckpointIDs(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turn-checkpoint-ids"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE (simulating agent mid-turn)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	state.TurnCheckpointIDs = nil // Start clean
	require.NoError(t, s.saveSessionState(state))

	// Create first commit with checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")

	err = s.PostCommit()
	require.NoError(t, err)

	// Verify TurnCheckpointIDs was populated
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, []string{"a1b2c3d4e5f6"}, state.TurnCheckpointIDs,
		"TurnCheckpointIDs should contain the checkpoint ID after condensation")
}

// TestPostCommit_IdleSession_DoesNotRecordTurnCheckpointIDs verifies that PostCommit
// does NOT record TurnCheckpointIDs for IDLE sessions.
func TestPostCommit_IdleSession_DoesNotRecordTurnCheckpointIDs(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-idle-no-turn-ids"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to IDLE with files touched so overlap check passes
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseIdle
	state.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(state))

	commitWithCheckpointTrailer(t, repo, dir, "c3d4e5f6a1b2")

	err = s.PostCommit()
	require.NoError(t, err)

	// Verify TurnCheckpointIDs was NOT set (IDLE sessions don't need finalization)
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Empty(t, state.TurnCheckpointIDs,
		"TurnCheckpointIDs should not be populated for IDLE sessions")
}

// TestHandleTurnEnd_PartialFailure verifies that HandleTurnEnd continues
// processing remaining checkpoints when one UpdateCommitted call fails.
// This locks the best-effort behavior: valid checkpoints get finalized even
// when one checkpoint ID is invalid or missing from entire/checkpoints/v1.
func TestHandleTurnEnd_PartialFailure(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-partial-failure"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE and create a transcript file with updated content
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	state.TurnCheckpointIDs = nil
	require.NoError(t, s.saveSessionState(state))

	// First commit → creates real checkpoint on entire/checkpoints/v1
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")
	require.NoError(t, s.PostCommit())

	// Write new content and create a second checkpoint on the shadow branch.
	// Use SaveStep directly (instead of setupSessionWithCheckpoint) so that
	// second.txt is included in FilesTouched — the overlap check needs it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "second.txt"), []byte("second file"), 0o644))
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	err = s.SaveStep(StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.txt"},
		NewFiles:       []string{"second.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 2",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err, "SaveStep should succeed for second checkpoint")
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	// Preserve TurnCheckpointIDs from the first commit
	state.TurnCheckpointIDs = []string{"a1b2c3d4e5f6"}
	require.NoError(t, s.saveSessionState(state))

	commitFilesWithTrailer(t, repo, dir, "b2c3d4e5f6a1", "second.txt")
	require.NoError(t, s.PostCommit())

	// Verify we now have 2 real checkpoint IDs
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Len(t, state.TurnCheckpointIDs, 2,
		"Should have 2 real checkpoint IDs after 2 mid-turn commits")

	// Inject a fake 3rd checkpoint ID that doesn't exist on entire/checkpoints/v1
	state.TurnCheckpointIDs = append(state.TurnCheckpointIDs, "ffffffffffff")

	// Write a full transcript file for HandleTurnEnd to read
	fullTranscript := `{"type":"human","message":{"content":"build something"}}
{"type":"assistant","message":{"content":"done building"}}
{"type":"human","message":{"content":"now test it"}}
{"type":"assistant","message":{"content":"tests pass"}}
`
	transcriptPath := filepath.Join(dir, ".entire", "metadata", sessionID, "full_transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(fullTranscript), 0o644))
	state.TranscriptPath = transcriptPath
	require.NoError(t, s.saveSessionState(state))

	// Call HandleTurnEnd — should NOT return error (best-effort)
	err = s.HandleTurnEnd(state)
	require.NoError(t, err,
		"HandleTurnEnd should return nil even with partial failures (best-effort)")

	// TurnCheckpointIDs should be cleared regardless of partial failure
	assert.Empty(t, state.TurnCheckpointIDs,
		"TurnCheckpointIDs should be cleared after HandleTurnEnd, even with errors")

	// Verify the 2 valid checkpoints were finalized with the full transcript
	store := checkpoint.NewGitStore(repo)
	for _, cpIDStr := range []string{"a1b2c3d4e5f6", "b2c3d4e5f6a1"} {
		cpID := id.MustCheckpointID(cpIDStr)
		content, readErr := store.ReadSessionContent(context.Background(), cpID, 0)
		require.NoError(t, readErr,
			"Should be able to read finalized checkpoint %s", cpIDStr)
		assert.Contains(t, string(content.Transcript), "now test it",
			"Checkpoint %s should contain the full transcript (including later messages)", cpIDStr)
	}
}

// setupSessionWithCheckpoint initializes a session and creates one checkpoint
// on the shadow branch so there is content available for condensation.
// Also modifies test.txt to "agent modified content" and includes it in the checkpoint,
// so content-aware carry-forward comparisons work correctly when commitFilesWithTrailer
// commits the same content.
func setupSessionWithCheckpoint(t *testing.T, s *ManualCommitStrategy, _ *git.Repository, dir, sessionID string) {
	t.Helper()

	// Modify test.txt with agent content (same content that commitFilesWithTrailer will commit)
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("agent modified content"), 0o644))

	// Create metadata directory with a transcript file
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"test prompt"}}
{"type":"assistant","message":{"content":"test response"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	// SaveStep creates the shadow branch and checkpoint
	// Include test.txt as a modified file so it's saved to the shadow branch
	err := s.SaveStep(StepContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{"test.txt"},
		NewFiles:       []string{},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err, "SaveStep should succeed to create shadow branch content")
}

// commitWithCheckpointTrailer creates a commit on the current branch with the
// Entire-Checkpoint trailer in the commit message. This simulates what happens
// after PrepareCommitMsg adds the trailer and the user completes the commit.
func commitWithCheckpointTrailer(t *testing.T, repo *git.Repository, dir, checkpointIDStr string) {
	t.Helper()
	commitFilesWithTrailer(t, repo, dir, checkpointIDStr, "test.txt")
}

// commitFilesWithTrailer stages the given files and commits with a checkpoint trailer.
// Files must already exist on disk. The test.txt file is modified to ensure there's always something to commit.
// Important: For tests using content-aware carry-forward, call setupSessionWithCheckpointAndFile first
// so the shadow branch has the same content that will be committed.
func commitFilesWithTrailer(t *testing.T, repo *git.Repository, dir, checkpointIDStr string, files ...string) {
	t.Helper()

	cpID := id.MustCheckpointID(checkpointIDStr)

	// Modify test.txt with agent-like content that matches what setupSessionWithCheckpointAndFile saves
	testFile := filepath.Join(dir, "test.txt")
	content := "agent modified content"
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)

	_, err = wt.Add("test.txt")
	require.NoError(t, err)
	for _, f := range files {
		_, err = wt.Add(f)
		require.NoError(t, err)
	}

	commitMsg := "test commit\n\n" + trailers.CheckpointTrailerKey + ": " + cpID.String() + "\n"
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@test.com",
			When:  time.Now(),
		},
	})
	require.NoError(t, err, "commit with checkpoint trailer should succeed")
}

// TestPostCommit_OldIdleSession_BaseCommitNotUpdated verifies that when an IDLE
// session from a previous commit exists, and a NEW session makes a commit, the
// old IDLE session's BaseCommit is NOT updated to the new HEAD.
//
// This is a regression test for the bug where old sessions (IDLE/ENDED) would
// have their BaseCommit updated, causing them to be incorrectly condensed on
// future commits because their BaseCommit matched the new shadow branch.
func TestPostCommit_OldIdleSession_BaseCommitNotUpdated(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// --- Create an old IDLE session from a previous commit ---
	oldSessionID := "old-idle-session"
	setupSessionWithCheckpoint(t, s, repo, dir, oldSessionID)

	oldState, err := s.loadSessionState(oldSessionID)
	require.NoError(t, err)
	oldState.Phase = session.PhaseIdle
	oldState.FilesTouched = []string{"old-file.txt"} // Has files touched (important for bug)
	require.NoError(t, s.saveSessionState(oldState))

	// Record the old session's BaseCommit BEFORE the new commit
	oldSessionOriginalBaseCommit := oldState.BaseCommit

	// Create a commit to move HEAD forward (simulating old session was condensed)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated"), 0o644))
	_, err = wt.Add("unrelated.txt")
	require.NoError(t, err)
	_, err = wt.Commit("unrelated commit without trailer", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// --- Create a NEW ACTIVE session at the new HEAD ---
	newSessionID := testNewActiveSessionID
	setupSessionWithCheckpoint(t, s, repo, dir, newSessionID)

	newState, err := s.loadSessionState(newSessionID)
	require.NoError(t, err)
	newState.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(newState))

	// --- Commit from the new session ---
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")

	// Get new HEAD for comparison
	head, err := repo.Head()
	require.NoError(t, err)
	newHead := head.Hash().String()

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// --- Verify: old IDLE session's BaseCommit should NOT be updated ---
	oldState, err = s.loadSessionState(oldSessionID)
	require.NoError(t, err)
	assert.Equal(t, oldSessionOriginalBaseCommit, oldState.BaseCommit,
		"OLD IDLE session's BaseCommit should NOT be updated when a different session commits")
	assert.NotEqual(t, newHead, oldState.BaseCommit,
		"OLD IDLE session's BaseCommit should NOT match new HEAD")

	// New ACTIVE session's BaseCommit SHOULD be updated (it was condensed)
	newState, err = s.loadSessionState(newSessionID)
	require.NoError(t, err)
	assert.Equal(t, newHead, newState.BaseCommit,
		"NEW ACTIVE session's BaseCommit should be updated after condensation")
}

// TestPostCommit_OldEndedSession_BaseCommitNotUpdated verifies that when an ENDED
// session from a previous commit exists (with no new content to condense), and a
// NEW session makes a commit, the old ENDED session's BaseCommit is NOT updated.
//
// This simulates the scenario where:
// 1. Old session ran and was already condensed (no new transcript content)
// 2. Old session is now ENDED
// 3. New session commits
// 4. Old ENDED session should NOT have BaseCommit updated
func TestPostCommit_OldEndedSession_BaseCommitNotUpdated(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// --- Create an old ENDED session that has NO new content to condense ---
	oldSessionID := "old-ended-session"
	setupSessionWithCheckpoint(t, s, repo, dir, oldSessionID)

	oldState, err := s.loadSessionState(oldSessionID)
	require.NoError(t, err)
	now := time.Now()
	oldState.Phase = session.PhaseEnded
	oldState.EndedAt = &now
	oldState.FilesTouched = []string{"old-file.txt"} // Has files touched
	// Mark transcript as fully condensed (no new content since last checkpoint)
	// The transcript has 2 lines, so CheckpointTranscriptStart=2 means no new content
	oldState.CheckpointTranscriptStart = 2
	require.NoError(t, s.saveSessionState(oldState))

	// Record the old session's BaseCommit BEFORE the new commit
	oldSessionOriginalBaseCommit := oldState.BaseCommit

	// Create a commit to move HEAD forward
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated"), 0o644))
	_, err = wt.Add("unrelated.txt")
	require.NoError(t, err)
	_, err = wt.Commit("unrelated commit without trailer", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// --- Create a NEW ACTIVE session at the new HEAD ---
	newSessionID := testNewActiveSessionID
	setupSessionWithCheckpoint(t, s, repo, dir, newSessionID)

	newState, err := s.loadSessionState(newSessionID)
	require.NoError(t, err)
	newState.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(newState))

	// --- Commit from the new session ---
	commitWithCheckpointTrailer(t, repo, dir, "b1c2d3e4f5a6")

	// Get new HEAD for comparison
	head, err := repo.Head()
	require.NoError(t, err)
	newHead := head.Hash().String()

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// --- Verify: old ENDED session's BaseCommit should NOT be updated ---
	oldState, err = s.loadSessionState(oldSessionID)
	require.NoError(t, err)
	assert.Equal(t, oldSessionOriginalBaseCommit, oldState.BaseCommit,
		"OLD ENDED session's BaseCommit should NOT be updated when a different session commits")
	assert.NotEqual(t, newHead, oldState.BaseCommit,
		"OLD ENDED session's BaseCommit should NOT match new HEAD")

	// New ACTIVE session's BaseCommit SHOULD be updated
	newState, err = s.loadSessionState(newSessionID)
	require.NoError(t, err)
	assert.Equal(t, newHead, newState.BaseCommit,
		"NEW ACTIVE session's BaseCommit should be updated after condensation")
}

// TestPostCommit_EndedSessionCarryForward_NotCondensedIntoUnrelatedCommit verifies
// that an ENDED session with carry-forward files is NOT condensed into a commit
// that doesn't touch any of those files.
//
// This is the primary bug scenario: ENDED sessions go through HandleCondenseIfFilesTouched,
// which previously only checked len(FilesTouched) > 0 && hasNew — no overlap check.
// Carry-forward would set FilesTouched with remaining uncommitted files, and
// sessionHasNewContent returned true because the shadow branch had content. This
// caused ENDED sessions to be re-condensed into every subsequent commit indefinitely.
func TestPostCommit_EndedSessionCarryForward_NotCondensedIntoUnrelatedCommit(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// --- Create an ENDED session with carry-forward files ---
	endedSessionID := "ended-carry-forward"
	setupSessionWithCheckpoint(t, s, repo, dir, endedSessionID)

	endedState, err := s.loadSessionState(endedSessionID)
	require.NoError(t, err)
	now := time.Now()
	endedState.Phase = session.PhaseEnded
	endedState.EndedAt = &now
	// Simulate carry-forward: session touched test.txt but it wasn't fully committed yet.
	// CheckpointTranscriptStart=0 so sessionHasNewContent returns true (transcript grew).
	endedState.FilesTouched = []string{"test.txt"}
	endedState.CheckpointTranscriptStart = 0
	require.NoError(t, s.saveSessionState(endedState))

	endedOriginalBaseCommit := endedState.BaseCommit
	endedOriginalStepCount := endedState.StepCount

	// Move HEAD forward with an unrelated commit (no trailer)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated work"), 0o644))
	_, err = wt.Add("unrelated.txt")
	require.NoError(t, err)
	_, err = wt.Commit("unrelated commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// --- Create a NEW ACTIVE session at the new HEAD ---
	newSessionID := testNewActiveSessionID
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new-feature.txt"), []byte("new feature content"), 0o644))

	metadataDir := ".entire/metadata/" + newSessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"add new feature"}}
{"type":"assistant","message":{"content":"adding new feature"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	err = s.SaveStep(StepContext{
		SessionID:      newSessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"new-feature.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint: new feature",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	newState, err := s.loadSessionState(newSessionID)
	require.NoError(t, err)
	newState.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(newState))

	// --- Commit ONLY new-feature.txt (not test.txt) with checkpoint trailer ---
	wt, err = repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("new-feature.txt")
	require.NoError(t, err)

	cpID := "ae1ae2ae3ae4"
	commitMsg := "add new feature\n\n" + trailers.CheckpointTrailerKey + ": " + cpID + "\n"
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	newHead := head.Hash().String()

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// --- Verify: ENDED session was NOT condensed ---
	endedState, err = s.loadSessionState(endedSessionID)
	require.NoError(t, err)

	// StepCount should be unchanged (not reset by condensation)
	assert.Equal(t, endedOriginalStepCount, endedState.StepCount,
		"ENDED session StepCount should NOT be reset (no condensation)")

	// BaseCommit should NOT be updated for ENDED sessions (PR #359)
	assert.Equal(t, endedOriginalBaseCommit, endedState.BaseCommit,
		"ENDED session BaseCommit should NOT be updated")

	// FilesTouched should still have the carry-forward files (not cleared by condensation)
	assert.Equal(t, []string{"test.txt"}, endedState.FilesTouched,
		"ENDED session FilesTouched should be preserved (carry-forward files not consumed)")

	// Phase stays ENDED
	assert.Equal(t, session.PhaseEnded, endedState.Phase,
		"ENDED session should remain ENDED")

	// --- Verify: new ACTIVE session WAS condensed ---
	newState, err = s.loadSessionState(newSessionID)
	require.NoError(t, err)
	assert.Equal(t, 0, newState.StepCount,
		"New ACTIVE session StepCount should be reset by condensation")
	assert.Equal(t, newHead, newState.BaseCommit,
		"New ACTIVE session BaseCommit should be updated after condensation")
}

// TestPostCommit_StaleActiveSession_NotCondensed verifies that a stale ACTIVE
// session (agent killed without Stop hook) is NOT condensed into an unrelated
// commit from a different session.
//
// Root cause: when an agent is killed without the Stop hook firing, its session
// remains in ACTIVE phase permanently. Previously, PostCommit unconditionally
// set hasNew=true for ACTIVE sessions and skipped the filesOverlapWithContent
// check, so stale ACTIVE sessions got condensed into every commit.
//
// The fix applies the overlap check to ALL sessions (including ACTIVE) using
// filesTouchedBefore, so stale sessions with unrelated files are filtered out.
func TestPostCommit_StaleActiveSession_NotCondensed(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// --- Create a stale ACTIVE session from an old commit ---
	// This simulates an agent that was killed without the Stop hook firing.
	staleSessionID := "stale-active-session"
	setupSessionWithCheckpoint(t, s, repo, dir, staleSessionID)

	staleState, err := s.loadSessionState(staleSessionID)
	require.NoError(t, err)
	staleState.Phase = session.PhaseActive
	// The stale session touched "test.txt" (set by setupSessionWithCheckpoint)
	// but the new commit will modify a different file.
	staleState.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(staleState))

	staleOriginalBaseCommit := staleState.BaseCommit
	staleOriginalStepCount := staleState.StepCount

	// Move HEAD forward with an unrelated commit (no trailer)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated work"), 0o644))
	_, err = wt.Add("unrelated.txt")
	require.NoError(t, err)
	_, err = wt.Commit("unrelated commit", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// --- Create a NEW ACTIVE session at the new HEAD ---
	newSessionID := testNewActiveSessionID

	// Create a new file for the new session (different from stale session's test.txt)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new-feature.txt"), []byte("new feature content"), 0o644))

	metadataDir := ".entire/metadata/" + newSessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := `{"type":"human","message":{"content":"add new feature"}}
{"type":"assistant","message":{"content":"adding new feature"}}
`
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	err = s.SaveStep(StepContext{
		SessionID:      newSessionID,
		ModifiedFiles:  []string{},
		NewFiles:       []string{"new-feature.txt"},
		DeletedFiles:   []string{},
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		CommitMessage:  "Checkpoint: new feature",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
	})
	require.NoError(t, err)

	newState, err := s.loadSessionState(newSessionID)
	require.NoError(t, err)
	newState.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(newState))

	// --- Commit ONLY new-feature.txt (not test.txt) with checkpoint trailer ---
	wt, err = repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("new-feature.txt")
	require.NoError(t, err)

	cpID := "de1de2de3de4"
	commitMsg := "add new feature\n\n" + trailers.CheckpointTrailerKey + ": " + cpID + "\n"
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	head, err := repo.Head()
	require.NoError(t, err)
	newHead := head.Hash().String()

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// --- Verify: stale ACTIVE session was NOT condensed ---
	staleState, err = s.loadSessionState(staleSessionID)
	require.NoError(t, err)

	// StepCount should be unchanged (not reset by condensation)
	assert.Equal(t, staleOriginalStepCount, staleState.StepCount,
		"Stale ACTIVE session StepCount should NOT be reset (no condensation)")

	// BaseCommit IS updated for ACTIVE sessions (updateBaseCommitIfChanged)
	assert.Equal(t, newHead, staleState.BaseCommit,
		"Stale ACTIVE session BaseCommit should be updated (ACTIVE sessions always get BaseCommit updated)")
	assert.NotEqual(t, staleOriginalBaseCommit, staleState.BaseCommit,
		"Stale ACTIVE session BaseCommit should have changed")

	// Phase stays ACTIVE
	assert.Equal(t, session.PhaseActive, staleState.Phase,
		"Stale ACTIVE session should remain ACTIVE")

	// --- Verify: new ACTIVE session WAS condensed ---
	newState, err = s.loadSessionState(newSessionID)
	require.NoError(t, err)

	// StepCount reset to 0 by condensation
	assert.Equal(t, 0, newState.StepCount,
		"New ACTIVE session StepCount should be reset by condensation")

	// BaseCommit updated to new HEAD
	assert.Equal(t, newHead, newState.BaseCommit,
		"New ACTIVE session BaseCommit should be updated after condensation")

	// Verify entire/checkpoints/v1 exists (new session was condensed)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err,
		"entire/checkpoints/v1 should exist (new session was condensed)")
}

// TestPostCommit_IdleSessionEmptyFilesTouched_NotCondensed verifies that an IDLE
// session with hasNew=true but empty FilesTouched is NOT condensed into a commit.
//
// This can happen for conversation-only sessions where the transcript grew but no
// files were modified. Previously, filesOverlapWithContent was called with an empty
// list and returned false. The shouldCondenseWithOverlapCheck method must also
// return false when filesTouchedBefore is empty.
func TestPostCommit_IdleSessionEmptyFilesTouched_NotCondensed(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}

	// --- Create an IDLE session with a checkpoint but no files touched ---
	idleSessionID := "idle-no-files-session"
	setupSessionWithCheckpoint(t, s, repo, dir, idleSessionID)

	idleState, err := s.loadSessionState(idleSessionID)
	require.NoError(t, err)
	idleState.Phase = session.PhaseIdle
	// Clear FilesTouched to simulate a conversation-only session
	idleState.FilesTouched = nil
	// CheckpointTranscriptStart=0 so sessionHasNewContent returns true
	idleState.CheckpointTranscriptStart = 0
	require.NoError(t, s.saveSessionState(idleState))

	idleOriginalStepCount := idleState.StepCount

	// --- Make a commit with an unrelated file ---
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other-work.txt"), []byte("other work"), 0o644))
	_, err = wt.Add("other-work.txt")
	require.NoError(t, err)

	cpID := "f1f2f3f4f5f6"
	commitMsg := "other work\n\n" + trailers.CheckpointTrailerKey + ": " + cpID + "\n"
	_, err = wt.Commit(commitMsg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
	})
	require.NoError(t, err)

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// --- Verify: IDLE session with no files was NOT condensed ---
	idleState, err = s.loadSessionState(idleSessionID)
	require.NoError(t, err)

	assert.Equal(t, idleOriginalStepCount, idleState.StepCount,
		"IDLE session with empty FilesTouched should NOT be condensed")
	assert.Equal(t, session.PhaseIdle, idleState.Phase,
		"IDLE session should remain IDLE")
	// BaseCommit is NOT updated for non-ACTIVE sessions (updateBaseCommitIfChanged skips them)
}
