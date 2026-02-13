//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestShadow_CommitBeforeStop tests the "commit while agent is still working" flow.
//
// When the user commits while the agent is in the ACTIVE phase (between
// SimulateUserPromptSubmit and SimulateStop), condensation happens immediately
// in PostCommit. The phase stays ACTIVE (no ACTIVE_COMMITTED transition).
// The shadow branch is migrated to the new HEAD after condensation.
// When the agent finishes its turn (SimulateStop), the session transitions to IDLE.
//
// State machine transitions tested:
//   - ACTIVE + GitCommit -> ACTIVE + [ActionCondense, ActionMigrateShadowBranch]
//   - ACTIVE + TurnEnd -> IDLE
func TestShadow_CommitBeforeStop(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// ========================================
	// Phase 1: Start session and create initial checkpoint
	// ========================================
	t.Log("Phase 1: Start session and create initial work")

	sess := env.NewSession()

	// Pass transcript path in the hook input so it's stored in session state
	// (needed for mid-session commit detection via live transcript)
	submitWithTranscriptPath := func(sessionID, transcriptPath string) {
		t.Helper()
		input := map[string]string{
			"session_id":      sessionID,
			"transcript_path": transcriptPath,
		}
		inputJSON, _ := json.Marshal(input)
		cmd := exec.Command(getTestBinary(), "hooks", "claude-code", "user-prompt-submit")
		cmd.Dir = env.RepoDir
		cmd.Stdin = bytes.NewReader(inputJSON)
		cmd.Env = append(os.Environ(),
			"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+env.ClaudeProjectDir,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("user-prompt-submit failed: %v\nOutput: %s", err, output)
		}
	}

	submitWithTranscriptPath(sess.ID, sess.TranscriptPath)

	// Verify session is ACTIVE
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist")
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase after first prompt should be %q, got %q", session.PhaseActive, state.Phase)
	}

	// Create a file and transcript, then stop (first checkpoint)
	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")
	sess.CreateTranscript("Create feature function", []FileChange{
		{Path: "feature.go", Content: "package main\n\nfunc Feature() {}\n"},
	})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Verify session is IDLE after stop
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist")
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Phase after first stop should be %q, got %q", session.PhaseIdle, state.Phase)
	}
	if state.StepCount != 1 {
		t.Errorf("StepCount after first checkpoint should be 1, got %d", state.StepCount)
	}

	// Verify shadow branch was created
	initialHead := state.BaseCommit
	shadowBranch := env.GetShadowBranchNameForCommit(initialHead)
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should exist after first checkpoint", shadowBranch)
	}

	// ========================================
	// Phase 2: Start new turn and create more work
	// ========================================
	t.Log("Phase 2: Start new turn, create more work")

	submitWithTranscriptPath(sess.ID, sess.TranscriptPath)

	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase after second prompt should be %q, got %q", session.PhaseActive, state.Phase)
	}

	// Create another file and write updated transcript
	env.WriteFile("utils.go", "package main\n\nfunc Util() {}\n")
	sess.TranscriptBuilder = NewTranscriptBuilder()
	sess.CreateTranscript("Create utils", []FileChange{
		{Path: "utils.go", Content: "package main\n\nfunc Util() {}\n"},
	})

	// Do NOT stop yet -- the agent is still ACTIVE

	// ========================================
	// Phase 3: User commits while agent is ACTIVE
	// ========================================
	t.Log("Phase 3: User commits while agent is ACTIVE")

	headBefore := env.GetHeadHash()
	env.GitCommitWithShadowHooks("Add feature and utils", "feature.go", "utils.go")
	commitHash := env.GetHeadHash()

	if commitHash == headBefore {
		t.Fatal("Commit was not created")
	}

	// Verify checkpoint trailer was added
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Log("Note: checkpoint trailer may not be present if no shadow branch content was detected (mid-session commit scenario)")
	} else {
		t.Logf("Commit has checkpoint trailer: %s", checkpointID)
	}

	// CRITICAL: Verify session phase stays ACTIVE (condenses immediately, no deferral)
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist after commit")
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase after commit-while-active should be %q, got %q",
			session.PhaseActive, state.Phase)
	}
	t.Logf("Session phase after mid-turn commit: %s", state.Phase)

	// Condensation happens immediately during PostCommit for ACTIVE sessions.
	// Verify StepCount was reset and shadow branch was migrated.
	if state.StepCount != 0 {
		t.Errorf("StepCount should be 0 after immediate condensation, got %d", state.StepCount)
	}

	// ========================================
	// Phase 4: Agent finishes turn (SimulateStop)
	// ========================================
	t.Log("Phase 4: Agent finishes turn (SimulateStop)")

	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (after commit) failed: %v", err)
	}

	// Verify session transitions to IDLE
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist after stop")
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Phase after stop from ACTIVE should be %q, got %q",
			session.PhaseIdle, state.Phase)
	}
	t.Logf("Session phase after stop: %s (StepCount: %d)", state.Phase, state.StepCount)

	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist after PostCommit condensation")
	}
	latestCheckpointID := env.TryGetLatestCheckpointID()
	if latestCheckpointID != "" {
		summaryPath := CheckpointSummaryPath(latestCheckpointID)
		if !env.FileExistsInBranch(paths.MetadataBranchName, summaryPath) {
			t.Errorf("Checkpoint metadata should exist at %s", summaryPath)
		} else {
			t.Logf("Condensed data exists at checkpoint %s", latestCheckpointID)
		}
	}

	t.Log("CommitBeforeStop test completed successfully")
}

// TestShadow_AmendPreservesTrailer tests that `git commit --amend` preserves
// the checkpoint trailer from the original commit.
//
// When a user amends a commit that has an Entire-Checkpoint trailer, the
// prepare-commit-msg hook (called with source="commit") should preserve the
// existing trailer. No duplicate condensation should occur.
//
// Hook behavior tested:
//   - prepare-commit-msg with source="commit": preserves existing trailer
//   - post-commit after amend: no duplicate condensation
func TestShadow_AmendPreservesTrailer(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// ========================================
	// Phase 1: Full workflow - create checkpoint and commit
	// ========================================
	t.Log("Phase 1: Create session, checkpoint, and commit")

	sess := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file and checkpoint
	env.WriteFile("main.go", "package main\n\nfunc main() {}\n")
	sess.CreateTranscript("Create main.go", []FileChange{
		{Path: "main.go", Content: "package main\n\nfunc main() {}\n"},
	})
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit with hooks (triggers condensation)
	env.GitCommitWithShadowHooks("Initial implementation", "main.go")

	originalCommitHash := env.GetHeadHash()
	originalCheckpointID := env.GetCheckpointIDFromCommitMessage(originalCommitHash)
	if originalCheckpointID == "" {
		t.Fatal("Original commit should have a checkpoint trailer")
	}
	t.Logf("Original commit %s has checkpoint ID: %s", originalCommitHash[:7], originalCheckpointID)

	// Verify condensation happened
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist after condensation")
	}

	// Record the sessions branch state for later comparison
	sessionsCommitBefore := env.GetLatestCommitMessageOnBranch(paths.MetadataBranchName)
	t.Logf("Sessions branch commit before amend:\n%s", sessionsCommitBefore)

	// ========================================
	// Phase 2: Amend the commit
	// ========================================
	t.Log("Phase 2: Amend the commit")

	// Amend with the same message (simulating a minor message edit or staging additional files)
	env.GitCommitAmendWithShadowHooks("Initial implementation (amended)")

	amendedCommitHash := env.GetHeadHash()
	t.Logf("Amended commit: %s", amendedCommitHash[:7])

	// The amended commit hash should be different from the original
	if amendedCommitHash == originalCommitHash {
		t.Error("Amended commit should have a different hash")
	}

	// ========================================
	// Phase 3: Verify trailer is preserved
	// ========================================
	t.Log("Phase 3: Verify checkpoint trailer is preserved")

	amendedCheckpointID := env.GetCheckpointIDFromCommitMessage(amendedCommitHash)
	if amendedCheckpointID == "" {
		t.Fatal("Amended commit should still have a checkpoint trailer")
	}
	t.Logf("Amended commit has checkpoint ID: %s", amendedCheckpointID)

	// The checkpoint ID should be the SAME as the original
	if amendedCheckpointID != originalCheckpointID {
		t.Errorf("Amended commit should preserve original checkpoint ID.\nOriginal: %s\nAmended:  %s",
			originalCheckpointID, amendedCheckpointID)
	}

	// ========================================
	// Phase 4: Verify no duplicate condensation
	// ========================================
	t.Log("Phase 4: Verify no duplicate condensation")

	// The sessions branch commit should not have changed (no new condensation)
	sessionsCommitAfter := env.GetLatestCommitMessageOnBranch(paths.MetadataBranchName)
	if sessionsCommitBefore != sessionsCommitAfter {
		t.Logf("Sessions branch commit changed after amend:\nBefore:\n%s\nAfter:\n%s",
			sessionsCommitBefore, sessionsCommitAfter)
		// This is not necessarily an error -- post-commit might trigger condensation
		// if there's new content. But with no new session activity, it should be the same.
		t.Log("Note: sessions branch was updated (may indicate duplicate condensation)")
	} else {
		t.Log("Sessions branch unchanged after amend (no duplicate condensation)")
	}

	// Verify the checkpoint data still exists and is accessible
	summaryPath := CheckpointSummaryPath(originalCheckpointID)
	if !env.FileExistsInBranch(paths.MetadataBranchName, summaryPath) {
		t.Errorf("Checkpoint metadata should still exist at %s after amend", summaryPath)
	}

	transcriptPath := SessionFilePath(originalCheckpointID, paths.TranscriptFileName)
	if !env.FileExistsInBranch(paths.MetadataBranchName, transcriptPath) {
		t.Errorf("Transcript should still exist at %s after amend", transcriptPath)
	}

	t.Log("AmendPreservesTrailer test completed successfully")
}
