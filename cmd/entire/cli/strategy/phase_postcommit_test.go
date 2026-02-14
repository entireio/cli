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

// TestPostCommit_ShadowBranch_PreservedWhenUncondensedActiveSessionExists verifies that
// the shadow branch is preserved when an ACTIVE session that was NOT condensed
// (e.g., it has no new content) still shares the branch with another session.
// Both sessions condense immediately on GitCommit, but the branch is only deleted
// when ALL sessions on it have been condensed.
func TestPostCommit_ShadowBranch_PreservedWhenUncondensedActiveSessionExists(t *testing.T) {
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
	// Because it has no new content, it will NOT be condensed, and its shadow branch must
	// be preserved so it can save checkpoints later.
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

	// Verify the IDLE session actually condensed (entire/checkpoints/v1 branch should exist)
	idleState, err = s.loadSessionState(idleSessionID)
	require.NoError(t, err)
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after IDLE session condensation")
	require.NotNil(t, sessionsRef)

	// Verify IDLE session's StepCount was reset by condensation
	assert.Equal(t, 0, idleState.StepCount,
		"IDLE session StepCount should be reset after condensation")

	// Verify shadow branch is preserved because the ACTIVE session was not condensed
	// (it had no new content) and still needs the branch for future checkpoints.
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	assert.NoError(t, err,
		"shadow branch should be preserved when an uncondensed active session still exists on it")
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

// TestPostCommit_IdleSession_NoNewContent_UpdatesBaseCommit verifies that when
// an IDLE session has no new transcript content since last condensation,
// PostCommit skips condensation but still updates BaseCommit.
func TestPostCommit_IdleSession_NoNewContent_UpdatesBaseCommit(t *testing.T) {
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

	// Record shadow branch name
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "f6a1b2c3d4e5")

	// Get new HEAD hash for comparison
	head, err := repo.Head()
	require.NoError(t, err)
	newHeadHash := head.Hash().String()

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify BaseCommit was updated to new HEAD
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, newHeadHash, state.BaseCommit,
		"BaseCommit should be updated to new HEAD when no new content")

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
// and updates BaseCommit via fallthrough.
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

	// Record shadow branch name and original StepCount
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "b2c3d4e5f6a2")

	// Get new HEAD hash
	head, err := repo.Head()
	require.NoError(t, err)
	newHeadHash := head.Hash().String()

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

	// BaseCommit should be updated to new HEAD via fallthrough
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, newHeadHash, state.BaseCommit,
		"BaseCommit should be updated to new HEAD via fallthrough")

	// StepCount unchanged
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged when no condensation happened")
}

// TestPostCommit_EndedSession_NoFilesTouched_Discards verifies that an ENDED
// session with no files touched takes the discard path, updating BaseCommit.
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

	// Record original StepCount
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "c3d4e5f6a1b3")

	// Get new HEAD hash
	head, err := repo.Head()
	require.NoError(t, err)
	newHeadHash := head.Hash().String()

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify entire/checkpoints/v1 branch does NOT exist (no condensation for discard path)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist for discard path")

	// BaseCommit should be updated to new HEAD
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, newHeadHash, state.BaseCommit,
		"BaseCommit should be updated to new HEAD on discard path")

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
	remaining := session.ApplyCommonActions(state, result)

	// Verify no strategy-specific actions for ACTIVE → IDLE
	assert.Empty(t, remaining,
		"ACTIVE + TurnEnd should not emit strategy-specific actions")

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

	err = s.SaveChanges(SaveContext{
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

	err = s.SaveChanges(SaveContext{
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
	err = s.SaveChanges(SaveContext{
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

	err = s.SaveChanges(SaveContext{
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

	// SaveChanges creates the shadow branch and checkpoint
	// Include test.txt as a modified file so it's saved to the shadow branch
	err := s.SaveChanges(SaveContext{
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
	require.NoError(t, err, "SaveChanges should succeed to create shadow branch content")
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
