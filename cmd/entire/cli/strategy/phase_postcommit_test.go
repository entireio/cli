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

const testSessionTranscript = `{"type":"human","message":{"content":"test prompt"}}
{"type":"assistant","message":{"content":"test response"}}
`

// TestPostCommit_ActiveSession_NoCondensation verifies that PostCommit on an
// ACTIVE session transitions to ACTIVE_COMMITTED without condensing.
// The shadow branch must be preserved because the session is still active.
func TestPostCommit_ActiveSession_NoCondensation(t *testing.T) {
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

	// Verify phase transitioned to ACTIVE_COMMITTED
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)
	assert.Equal(t, session.PhaseActiveCommitted, state.Phase,
		"ACTIVE session should transition to ACTIVE_COMMITTED on GitCommit")

	// Verify shadow branch is NOT deleted (session is still active).
	// After PostCommit, BaseCommit is updated to new HEAD, so use the current state.
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	assert.NoError(t, err,
		"shadow branch should be preserved when session is still active")
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

// TestPostCommit_ShadowBranch_PreservedWhenActiveSessionExists verifies that
// the shadow branch is preserved when ANY session on it is still active,
// even if another session on the same branch is IDLE and gets condensed.
func TestPostCommit_ShadowBranch_PreservedWhenActiveSessionExists(t *testing.T) {
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
	require.NoError(t, s.saveSessionState(idleState))

	// Create a second session with the SAME base commit and worktree (concurrent session)
	// Save the active session with ACTIVE phase and some checkpoints
	now := time.Now()
	activeState := &SessionState{
		SessionID:           activeSessionID,
		BaseCommit:          baseCommit,
		WorktreePath:        worktreePath,
		WorktreeID:          worktreeID,
		StartedAt:           now,
		Phase:               session.PhaseActive,
		LastInteractionTime: &now,
		StepCount:           1,
	}
	require.NoError(t, s.saveSessionState(activeState))

	// Create a commit WITH the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "d4e5f6a1b2c3")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify the ACTIVE session's phase is now ACTIVE_COMMITTED
	activeState, err = s.loadSessionState(activeSessionID)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActiveCommitted, activeState.Phase,
		"ACTIVE session should transition to ACTIVE_COMMITTED on GitCommit")

	// Verify the IDLE session actually condensed (entire/checkpoints/v1 branch should exist)
	idleState, err = s.loadSessionState(idleSessionID)
	require.NoError(t, err)
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after IDLE session condensation")
	require.NotNil(t, sessionsRef)

	// Verify IDLE session's StepCount was reset by condensation
	assert.Equal(t, 0, idleState.StepCount,
		"IDLE session StepCount should be reset after condensation")

	// Verify shadow branch is NOT deleted because the ACTIVE session still needs it.
	// After PostCommit, BaseCommit is updated to new HEAD via migration.
	newShadowBranch := getShadowBranchNameForCommit(activeState.BaseCommit, activeState.WorktreeID)
	refName := plumbing.NewBranchReferenceName(newShadowBranch)
	_, err = repo.Reference(refName, true)
	assert.NoError(t, err,
		"shadow branch should be preserved when an active session still exists on it")
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

// TestPostCommit_ActiveCommitted_MigratesShadowBranch verifies that an
// ACTIVE_COMMITTED session receiving another commit migrates the shadow branch
// to the new HEAD and stays in ACTIVE_COMMITTED.
func TestPostCommit_ActiveCommitted_MigratesShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-ac-migrate"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE_COMMITTED
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActiveCommitted
	require.NoError(t, s.saveSessionState(state))

	// Record original shadow branch name and BaseCommit
	originalShadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	originalStepCount := state.StepCount

	// Create a commit with the checkpoint trailer
	commitWithCheckpointTrailer(t, repo, dir, "d4e5f6a1b2c4")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify phase stays ACTIVE_COMMITTED
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActiveCommitted, state.Phase,
		"ACTIVE_COMMITTED session should stay ACTIVE_COMMITTED on subsequent commit")

	// Verify BaseCommit updated to new HEAD
	head, err := repo.Head()
	require.NoError(t, err)
	assert.Equal(t, head.Hash().String(), state.BaseCommit,
		"BaseCommit should be updated to new HEAD after migration")

	// Verify new shadow branch exists at new HEAD hash
	newShadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	newRefName := plumbing.NewBranchReferenceName(newShadowBranch)
	_, err = repo.Reference(newRefName, true)
	require.NoError(t, err,
		"new shadow branch should exist after migration")

	// Verify original shadow branch no longer exists (was migrated/renamed)
	oldRefName := plumbing.NewBranchReferenceName(originalShadowBranch)
	_, err = repo.Reference(oldRefName, true)
	require.Error(t, err,
		"original shadow branch should no longer exist after migration")

	// StepCount unchanged (no condensation)
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should be unchanged - no condensation for ACTIVE_COMMITTED")

	// entire/checkpoints/v1 branch should NOT exist (no condensation)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist - no condensation for ACTIVE_COMMITTED")
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

// TestPostCommit_ActiveSession_SetsPendingCheckpointID verifies that PostCommit
// stores PendingCheckpointID when transitioning ACTIVE → ACTIVE_COMMITTED.
// This ensures HandleTurnEnd can reuse the same checkpoint ID that's in the
// commit trailer, rather than generating a mismatched one.
func TestPostCommit_ActiveSession_SetsPendingCheckpointID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-postcommit-pending-cpid"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Set phase to ACTIVE (agent mid-turn)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	state.PendingCheckpointID = "" // Ensure it starts empty
	require.NoError(t, s.saveSessionState(state))

	// Create a commit with a known checkpoint ID
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")

	// Run PostCommit
	err = s.PostCommit()
	require.NoError(t, err)

	// Verify phase transitioned to ACTIVE_COMMITTED
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state.Phase)

	// Verify PendingCheckpointID was stored from the commit trailer
	assert.Equal(t, "a1b2c3d4e5f6", state.PendingCheckpointID,
		"PendingCheckpointID should be set to the commit's checkpoint ID for deferred condensation")
}

// TestTurnEnd_ActiveCommitted_ReusesCheckpointID verifies that HandleTurnEnd
// uses PendingCheckpointID (set by PostCommit) rather than generating a new one.
// This ensures the condensed metadata matches the commit trailer.
func TestTurnEnd_ActiveCommitted_ReusesCheckpointID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turnend-reuses-cpid"

	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Simulate PostCommit: transition to ACTIVE_COMMITTED with PendingCheckpointID
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	commitWithCheckpointTrailer(t, repo, dir, "d4e5f6a1b2c3")

	err = s.PostCommit()
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, "d4e5f6a1b2c3", state.PendingCheckpointID)

	// Run TurnEnd
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state, result)

	err = s.HandleTurnEnd(state, remaining)
	require.NoError(t, err)

	// Verify the condensed checkpoint ID matches the commit trailer
	// The LastCheckpointID is set by condenseAndUpdateState on success
	assert.Equal(t, id.CheckpointID("d4e5f6a1b2c3"), state.LastCheckpointID,
		"condensation should use PendingCheckpointID, not generate a new one")
}

// TestTurnEnd_ConcurrentSession_PreservesShadowBranch verifies that
// HandleTurnEnd does NOT delete the shadow branch when another active
// session shares it.
func TestTurnEnd_ConcurrentSession_PreservesShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID1 := "test-turnend-concurrent-1"
	sessionID2 := "test-turnend-concurrent-2"

	// Initialize first session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID1)

	// Get worktree info from first session
	state1, err := s.loadSessionState(sessionID1)
	require.NoError(t, err)
	worktreePath := state1.WorktreePath
	baseCommit := state1.BaseCommit
	worktreeID := state1.WorktreeID

	// Transition first session through PostCommit to ACTIVE_COMMITTED
	state1.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state1))

	commitWithCheckpointTrailer(t, repo, dir, "e5f6a1b2c3d4")

	err = s.PostCommit()
	require.NoError(t, err)

	state1, err = s.loadSessionState(sessionID1)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state1.Phase)

	// Create a second session with the SAME base commit and worktree (concurrent)
	now := time.Now()
	state2 := &SessionState{
		SessionID:           sessionID2,
		BaseCommit:          state1.BaseCommit, // Same base commit (post-migration)
		WorktreePath:        worktreePath,
		WorktreeID:          worktreeID,
		StartedAt:           now,
		Phase:               session.PhaseActive,
		LastInteractionTime: &now,
		StepCount:           1,
	}
	require.NoError(t, s.saveSessionState(state2))

	// Record shadow branch name (shared by both sessions)
	shadowBranch := getShadowBranchNameForCommit(state1.BaseCommit, state1.WorktreeID)

	// First session ends its turn — should NOT delete shadow branch
	result := session.Transition(state1.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state1, result)

	err = s.HandleTurnEnd(state1, remaining)
	require.NoError(t, err)

	// Shadow branch at the pre-condensation BaseCommit should be preserved
	// because session2 is still active on it.
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	require.NoError(t, err,
		"shadow branch should be preserved when another active session shares it")

	// Condensation still succeeded for session1
	assert.Equal(t, 0, state1.StepCount,
		"StepCount should be reset after condensation")
	assert.Equal(t, session.PhaseIdle, state1.Phase,
		"first session should be IDLE after turn end")

	// Second session is still active and unaffected
	state2, err = s.loadSessionState(sessionID2)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseActive, state2.Phase,
		"second session should still be ACTIVE")
	_ = baseCommit // used for documentation
}

// TestTurnEnd_ActiveCommitted_CondensesSession verifies that HandleTurnEnd
// with ActionCondense (from ACTIVE_COMMITTED → IDLE) condenses the session
// to entire/checkpoints/v1 and cleans up the shadow branch.
func TestTurnEnd_ActiveCommitted_CondensesSession(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turnend-condenses"

	// Initialize session and save a checkpoint so there is shadow branch content
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Simulate PostCommit: create a commit with trailer and transition to ACTIVE_COMMITTED
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")

	// Run PostCommit so phase transitions to ACTIVE_COMMITTED and PendingCheckpointID is set
	err = s.PostCommit()
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state.Phase)

	// Now simulate the TurnEnd transition that the handler dispatches
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state, result)

	// Verify the state machine emits ActionCondense
	require.Contains(t, remaining, session.ActionCondense,
		"ACTIVE_COMMITTED + TurnEnd should emit ActionCondense")

	// Record shadow branch name BEFORE HandleTurnEnd (BaseCommit may change)
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)

	// Call HandleTurnEnd with the remaining actions
	err = s.HandleTurnEnd(state, remaining)
	require.NoError(t, err)

	// Verify condensation happened: entire/checkpoints/v1 branch should exist
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/checkpoints/v1 branch should exist after turn-end condensation")
	assert.NotNil(t, sessionsRef)

	// Verify shadow branch IS deleted after condensation
	refName := plumbing.NewBranchReferenceName(shadowBranch)
	_, err = repo.Reference(refName, true)
	require.Error(t, err,
		"shadow branch should be deleted after turn-end condensation")

	// Verify StepCount was reset by condensation
	assert.Equal(t, 0, state.StepCount,
		"StepCount should be reset after condensation")

	// Verify phase is IDLE (set by ApplyCommonActions above)
	assert.Equal(t, session.PhaseIdle, state.Phase,
		"phase should be IDLE after TurnEnd")
}

// TestTurnEnd_ActiveCommitted_CondensationFailure_PreservesShadowBranch verifies
// that when HandleTurnEnd condensation fails, BaseCommit is NOT updated and
// the shadow branch is preserved.
func TestTurnEnd_ActiveCommitted_CondensationFailure_PreservesShadowBranch(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-turnend-condense-fail"

	// Initialize session and save a checkpoint
	setupSessionWithCheckpoint(t, s, repo, dir, sessionID)

	// Simulate PostCommit: transition to ACTIVE_COMMITTED
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.Phase = session.PhaseActive
	require.NoError(t, s.saveSessionState(state))

	commitWithCheckpointTrailer(t, repo, dir, "b2c3d4e5f6a1")

	err = s.PostCommit()
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state.Phase)

	// Record original BaseCommit before corruption
	originalBaseCommit := state.BaseCommit
	originalStepCount := state.StepCount

	// Corrupt shadow branch by pointing it at ZeroHash
	shadowBranch := getShadowBranchNameForCommit(state.BaseCommit, state.WorktreeID)
	corruptRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadowBranch), plumbing.ZeroHash)
	require.NoError(t, repo.Storer.SetReference(corruptRef))

	// Run the transition
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state, result)

	// Call HandleTurnEnd — condensation should fail silently
	err = s.HandleTurnEnd(state, remaining)
	require.NoError(t, err, "HandleTurnEnd should not return error even when condensation fails")

	// BaseCommit should NOT be updated (condensation failed)
	assert.Equal(t, originalBaseCommit, state.BaseCommit,
		"BaseCommit should NOT be updated when condensation fails")
	assert.Equal(t, originalStepCount, state.StepCount,
		"StepCount should NOT be reset when condensation fails")

	// entire/checkpoints/v1 branch should NOT exist (condensation failed)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	assert.Error(t, err,
		"entire/checkpoints/v1 branch should NOT exist when condensation fails")
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

	// Call HandleTurnEnd with empty actions — should be a no-op
	err = s.HandleTurnEnd(state, remaining)
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

// TestTurnEnd_DeferredCondensation_AttributionUsesOriginalBase verifies that
// deferred condensation (ACTIVE_COMMITTED → IDLE) uses AttributionBaseCommit
// instead of BaseCommit for attribution, so the diff is non-zero.
//
// Scenario: agent modifies a file, user commits mid-turn, then turn ends.
// Without the fix, BaseCommit is updated to the new HEAD by PostCommit migration,
// so baseTree == headTree and attribution shows zero changes.
// With the fix, AttributionBaseCommit preserves the original base commit.
func TestTurnEnd_DeferredCondensation_AttributionUsesOriginalBase(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	s := &ManualCommitStrategy{}
	sessionID := "test-deferred-attribution"

	// Initialize session and save a checkpoint that includes a modified file.
	// The "agent" modifies test.txt before saving the checkpoint.
	setupSessionWithFileChange(t, s, repo, dir, sessionID)

	// Record the original base commit (commit A)
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	originalBaseCommit := state.BaseCommit

	// Verify AttributionBaseCommit is set at session init
	assert.Equal(t, originalBaseCommit, state.AttributionBaseCommit,
		"AttributionBaseCommit should equal BaseCommit at session start")

	// Set phase to ACTIVE (simulating agent mid-turn)
	state.Phase = session.PhaseActive
	state.FilesTouched = []string{"test.txt"}
	require.NoError(t, s.saveSessionState(state))

	// User commits (creates commit B). This triggers PostCommit which:
	// - Transitions ACTIVE → ACTIVE_COMMITTED (defers condensation)
	// - Migrates shadow branch to new HEAD
	// - Updates BaseCommit to new HEAD
	commitWithCheckpointTrailer(t, repo, dir, "a1b2c3d4e5f6")

	err = s.PostCommit()
	require.NoError(t, err)

	// Reload state and verify the key invariant:
	// BaseCommit has moved to the new HEAD, but AttributionBaseCommit stays at original
	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.Equal(t, session.PhaseActiveCommitted, state.Phase)

	head, err := repo.Head()
	require.NoError(t, err)
	newHeadHash := head.Hash().String()

	assert.Equal(t, newHeadHash, state.BaseCommit,
		"BaseCommit should be updated to new HEAD after migration")
	assert.Equal(t, originalBaseCommit, state.AttributionBaseCommit,
		"AttributionBaseCommit should still point to original base (commit A)")
	assert.NotEqual(t, state.BaseCommit, state.AttributionBaseCommit,
		"BaseCommit and AttributionBaseCommit should diverge after mid-turn commit")

	// Now simulate TurnEnd (agent finishes) — deferred condensation runs
	result := session.Transition(state.Phase, session.EventTurnEnd, session.TransitionContext{})
	remaining := session.ApplyCommonActions(state, result)
	require.Contains(t, remaining, session.ActionCondense)

	err = s.HandleTurnEnd(state, remaining)
	require.NoError(t, err)

	// After condensation, verify AttributionBaseCommit is updated to match BaseCommit
	assert.Equal(t, state.BaseCommit, state.AttributionBaseCommit,
		"AttributionBaseCommit should be updated after successful condensation")

	// Verify condensation actually happened
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "entire/sessions branch should exist after deferred condensation")

	// Read back the committed metadata and verify attribution is non-zero.
	// The agent modified test.txt (added a line), so AgentLines should be > 0.
	store := checkpoint.NewGitStore(repo)
	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err, "should be able to read condensed session content")
	require.NotNil(t, content)
	require.NotNil(t, content.Metadata.InitialAttribution,
		"condensed metadata should include attribution")
	assert.Positive(t, content.Metadata.InitialAttribution.TotalCommitted,
		"attribution TotalCommitted should be non-zero (agent modified test.txt)")
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

	// --- Commit and condense (round 1) ---
	checkpointID1 := "a1a2a3a4a5a6"
	commitWithCheckpointTrailer(t, repo, dir, checkpointID1)

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
		"FilesTouched should be nil after condensation")

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

	// --- Commit and condense (round 2) ---
	checkpointID2 := "b1b2b3b4b5b6"
	commitWithCheckpointTrailer(t, repo, dir, checkpointID2)

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

// setupSessionWithFileChange is like setupSessionWithCheckpoint but also modifies
// test.txt so the shadow branch checkpoint includes actual file changes.
// This enables attribution testing: the diff between base commit and the
// checkpoint/HEAD shows real line changes.
func setupSessionWithFileChange(t *testing.T, s *ManualCommitStrategy, _ *git.Repository, dir, sessionID string) {
	t.Helper()

	// Modify test.txt to simulate agent work (adds lines relative to initial commit)
	testFile := filepath.Join(dir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("initial content\nagent added line\n"), 0o644))

	// Create metadata directory with a transcript file
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := testSessionTranscript
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	// SaveChanges creates the shadow branch and checkpoint
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

// setupSessionWithCheckpoint initializes a session and creates one checkpoint
// on the shadow branch so there is content available for condensation.
func setupSessionWithCheckpoint(t *testing.T, s *ManualCommitStrategy, _ *git.Repository, dir, sessionID string) {
	t.Helper()

	// Create metadata directory with a transcript file
	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	transcript := testSessionTranscript
	require.NoError(t, os.WriteFile(
		filepath.Join(metadataDirAbs, paths.TranscriptFileName),
		[]byte(transcript), 0o644))

	// SaveChanges creates the shadow branch and checkpoint
	err := s.SaveChanges(SaveContext{
		SessionID:      sessionID,
		ModifiedFiles:  []string{},
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

	cpID := id.MustCheckpointID(checkpointIDStr)

	// Modify a file so there is something to commit
	testFile := filepath.Join(dir, "test.txt")
	content := "updated at " + time.Now().String()
	require.NoError(t, os.WriteFile(testFile, []byte(content), 0o644))

	wt, err := repo.Worktree()
	require.NoError(t, err)

	_, err = wt.Add("test.txt")
	require.NoError(t, err)

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
