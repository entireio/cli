//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestShadow_DeferredTranscriptFinalization tests that HandleTurnEnd updates
// the provisional transcript (written at commit time) with the full transcript
// (available at turn end).
//
// Flow:
// 1. Agent starts working (ACTIVE)
// 2. Agent makes file changes
// 3. User commits while agent is ACTIVE → provisional transcript condensed
// 4. Agent continues work (updates transcript)
// 5. Agent finishes (SimulateStop) → transcript finalized via UpdateCommitted
//
// This verifies that the final transcript on entire/checkpoints/v1 includes
// work done AFTER the commit.
func TestShadow_DeferredTranscriptFinalization(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session (ACTIVE)
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Expected ACTIVE phase, got %s", state.Phase)
	}

	// Create file and initial transcript
	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")
	sess.CreateTranscript("Create feature function", []FileChange{
		{Path: "feature.go", Content: "package main\n\nfunc Feature() {}\n"},
	})

	// Debug: verify session state before commit
	preCommitState, _ := env.GetSessionState(sess.ID)
	if preCommitState == nil {
		t.Fatal("Session state should exist before commit")
	}
	t.Logf("Pre-commit session state: phase=%s, worktreePath=%s, baseCommit=%s",
		preCommitState.Phase, preCommitState.WorktreePath, preCommitState.BaseCommit[:7])

	// User commits while agent is still ACTIVE
	// This triggers condensation with the provisional transcript
	// Using custom commit with verbose output for debugging
	{
		env.GitAdd("feature.go")
		msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
		if err := os.WriteFile(msgFile, []byte("Add feature"), 0o644); err != nil {
			t.Fatalf("failed to write commit message: %v", err)
		}

		// Run prepare-commit-msg
		prepCmd := exec.Command(getTestBinary(), "hooks", "git", "prepare-commit-msg", msgFile, "message")
		prepCmd.Dir = env.RepoDir
		prepCmd.Env = append(os.Environ(), "ENTIRE_TEST_TTY=1")
		prepOutput, prepErr := prepCmd.CombinedOutput()
		t.Logf("prepare-commit-msg output: %s (err: %v)", prepOutput, prepErr)

		// Read modified message
		modifiedMsg, _ := os.ReadFile(msgFile)
		t.Logf("Commit message after prepare-commit-msg: %s", modifiedMsg)

		// Create commit
		repo, _ := git.PlainOpen(env.RepoDir)
		worktree, _ := repo.Worktree()
		_, err := worktree.Commit(string(modifiedMsg), &git.CommitOptions{
			Author: &object.Signature{
				Name:  "Test User",
				Email: "test@example.com",
				When:  time.Now(),
			},
		})
		if err != nil {
			t.Fatalf("failed to commit: %v", err)
		}

		// Run post-commit
		postCmd := exec.Command(getTestBinary(), "hooks", "git", "post-commit")
		postCmd.Dir = env.RepoDir
		postOutput, postErr := postCmd.CombinedOutput()
		t.Logf("post-commit output: %s (err: %v)", postOutput, postErr)
	}
	commitHash := env.GetHeadHash()

	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Fatal("Commit should have checkpoint trailer")
	}
	t.Logf("Checkpoint ID after mid-session commit: %s", checkpointID)

	// Debug: verify session state after commit
	postCommitState, _ := env.GetSessionState(sess.ID)
	if postCommitState != nil {
		t.Logf("Post-commit session state: phase=%s, baseCommit=%s, turnCheckpointIDs=%v",
			postCommitState.Phase, postCommitState.BaseCommit[:7], postCommitState.TurnCheckpointIDs)
	} else {
		t.Log("Post-commit session state is nil (shouldn't happen)")
	}

	// Debug: list all branches
	branches := env.ListBranchesWithPrefix("")
	t.Logf("All branches after commit: %v", branches)

	// Verify checkpoint exists on metadata branch (provisional)
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist")
	}

	// Read the provisional transcript
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	provisionalContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Fatalf("Provisional transcript should exist at %s", transcriptPath)
	}
	t.Logf("Provisional transcript length: %d bytes", len(provisionalContent))

	// Verify session state has TurnCheckpointIDs for deferred finalization
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if len(state.TurnCheckpointIDs) == 0 {
		t.Error("TurnCheckpointIDs should contain the checkpoint ID for finalization")
	}

	// Agent continues work - add more to transcript
	sess.TranscriptBuilder.AddUserMessage("Also add a helper function")
	sess.TranscriptBuilder.AddAssistantMessage("Adding helper function now")
	toolID := sess.TranscriptBuilder.AddToolUse("mcp__acp__Write", "helper.go", "package main\n\nfunc Helper() {}\n")
	sess.TranscriptBuilder.AddToolResult(toolID)
	sess.TranscriptBuilder.AddAssistantMessage("Done with both changes!")

	// Write updated transcript
	if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
		t.Fatalf("Failed to write updated transcript: %v", err)
	}

	// Also create the file in worktree (for consistency, though not committed yet)
	env.WriteFile("helper.go", "package main\n\nfunc Helper() {}\n")

	// Agent finishes turn - this triggers HandleTurnEnd which should finalize the transcript
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Read the finalized transcript
	finalContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Fatalf("Finalized transcript should exist at %s", transcriptPath)
	}
	t.Logf("Finalized transcript length: %d bytes", len(finalContent))

	// The finalized transcript should be longer (include post-commit work)
	if len(finalContent) <= len(provisionalContent) {
		t.Errorf("Finalized transcript should be longer than provisional.\n"+
			"Provisional: %d bytes\nFinalized: %d bytes",
			len(provisionalContent), len(finalContent))
	}

	// Verify the finalized transcript contains the additional work
	if !strings.Contains(finalContent, "Also add a helper function") {
		t.Error("Finalized transcript should contain post-commit user message")
	}
	if !strings.Contains(finalContent, "helper.go") {
		t.Error("Finalized transcript should contain helper.go tool use")
	}

	// Verify session is now IDLE
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Expected IDLE phase after stop, got %s", state.Phase)
	}

	// TurnCheckpointIDs should be cleared after finalization
	if len(state.TurnCheckpointIDs) != 0 {
		t.Errorf("TurnCheckpointIDs should be cleared after finalization, got %v", state.TurnCheckpointIDs)
	}

	t.Log("DeferredTranscriptFinalization test completed successfully")
}

// TestShadow_CarryForward_ActiveSession tests that when a user commits only
// some of the files touched by an ACTIVE session, the remaining files are
// carried forward to a new shadow branch.
//
// Flow:
// 1. Agent touches files A, B, C while ACTIVE
// 2. User commits only file A → checkpoint #1
// 3. Session remains ACTIVE with files B, C pending
// 4. User commits file B → checkpoint #2 (new checkpoint ID)
func TestShadow_CarryForward_ActiveSession(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session (ACTIVE)
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Create multiple files
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")
	env.WriteFile("fileC.go", "package main\n\nfunc C() {}\n")

	// Create transcript with all files
	sess.CreateTranscript("Create files A, B, and C", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
		{Path: "fileC.go", Content: "package main\n\nfunc C() {}\n"},
	})

	// Verify session is ACTIVE
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Expected ACTIVE phase, got %s", state.Phase)
	}

	// First commit: only file A
	env.GitCommitWithShadowHooks("Add file A", "fileA.go")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have checkpoint trailer")
	}
	t.Logf("First checkpoint ID: %s", firstCheckpointID)

	// Session should still be ACTIVE (mid-turn commit)
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Expected ACTIVE phase after partial commit, got %s", state.Phase)
	}
	t.Logf("After first commit: FilesTouched=%v, CheckpointTranscriptStart=%d, BaseCommit=%s, TurnCheckpointIDs=%v",
		state.FilesTouched, state.CheckpointTranscriptStart, state.BaseCommit[:7], state.TurnCheckpointIDs)

	// List branches to see if shadow branch was created
	branches := env.ListBranchesWithPrefix("entire/")
	t.Logf("Entire branches after first commit: %v", branches)

	// Stage file B to see what the commit would include
	env.GitAdd("fileB.go")

	// Second commit: file B (should get a NEW checkpoint ID)
	env.GitCommitWithShadowHooks("Add file B", "fileB.go")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)
	if secondCheckpointID == "" {
		t.Fatal("Second commit should have checkpoint trailer")
	}
	t.Logf("Second checkpoint ID: %s", secondCheckpointID)

	// CRITICAL: Each commit gets its own unique checkpoint ID
	if firstCheckpointID == secondCheckpointID {
		t.Errorf("Each commit should get a unique checkpoint ID.\n"+
			"First: %s\nSecond: %s",
			firstCheckpointID, secondCheckpointID)
	}

	// Verify both checkpoints exist on metadata branch
	firstPath := CheckpointSummaryPath(firstCheckpointID)
	if !env.FileExistsInBranch(paths.MetadataBranchName, firstPath) {
		t.Errorf("First checkpoint metadata should exist at %s", firstPath)
	}

	secondPath := CheckpointSummaryPath(secondCheckpointID)
	if !env.FileExistsInBranch(paths.MetadataBranchName, secondPath) {
		t.Errorf("Second checkpoint metadata should exist at %s", secondPath)
	}

	t.Log("CarryForward_ActiveSession test completed successfully")
}

// TestShadow_CarryForward_IdleSession tests that when a user commits only
// some of the files touched during an IDLE session, subsequent commits
// for remaining files can still get checkpoint trailers.
//
// Flow:
//  1. Agent touches files A and B, then stops (IDLE)
//  2. User commits only file A → checkpoint #1
//  3. Session is IDLE, but still has file B pending
//  4. User commits file B → checkpoint #2 (if carry-forward for IDLE is implemented)
//     or no trailer (if IDLE sessions don't carry forward)
func TestShadow_CarryForward_IdleSession(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create multiple files
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")

	sess.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
	})

	// Stop session (becomes IDLE)
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Expected IDLE phase, got %s", state.Phase)
	}

	// First commit: only file A
	env.GitCommitWithShadowHooks("Add file A", "fileA.go")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have checkpoint trailer (IDLE session, files overlap)")
	}
	t.Logf("First checkpoint ID: %s", firstCheckpointID)

	// Second commit: file B
	// In the 1:1 model, this should also get a checkpoint if IDLE sessions
	// carry forward, or no trailer if they don't.
	env.GitCommitWithShadowHooks("Add file B", "fileB.go")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)

	if secondCheckpointID != "" {
		// If carry-forward is implemented for IDLE sessions
		t.Logf("Second checkpoint ID: %s (carry-forward active)", secondCheckpointID)
		if firstCheckpointID == secondCheckpointID {
			t.Error("If both commits have trailers, they must have DIFFERENT checkpoint IDs")
		}
	} else {
		// If IDLE sessions don't carry forward (current behavior)
		t.Log("Second commit has no checkpoint trailer (IDLE sessions don't carry forward)")
	}

	t.Log("CarryForward_IdleSession test completed successfully")
}

// TestShadow_MultipleCommits_SameActiveTurn tests that multiple commits
// during a single ACTIVE turn each get unique checkpoint IDs, and all
// are finalized when the turn ends.
//
// Flow:
// 1. Agent starts working (ACTIVE)
// 2. User commits file A → checkpoint #1 (provisional)
// 3. User commits file B → checkpoint #2 (provisional)
// 4. User commits file C → checkpoint #3 (provisional)
// 5. Agent finishes (SimulateStop) → all 3 checkpoints finalized
func TestShadow_MultipleCommits_SameActiveTurn(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session (ACTIVE)
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Create multiple files
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")
	env.WriteFile("fileC.go", "package main\n\nfunc C() {}\n")

	sess.CreateTranscript("Create files A, B, and C", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
		{Path: "fileC.go", Content: "package main\n\nfunc C() {}\n"},
	})

	// Commit each file separately while ACTIVE
	checkpointIDs := make([]string, 3)

	env.GitCommitWithShadowHooks("Add file A", "fileA.go")
	checkpointIDs[0] = env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointIDs[0] == "" {
		t.Fatal("First commit should have checkpoint trailer")
	}

	env.GitCommitWithShadowHooks("Add file B", "fileB.go")
	checkpointIDs[1] = env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointIDs[1] == "" {
		t.Fatal("Second commit should have checkpoint trailer")
	}

	env.GitCommitWithShadowHooks("Add file C", "fileC.go")
	checkpointIDs[2] = env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointIDs[2] == "" {
		t.Fatal("Third commit should have checkpoint trailer")
	}

	t.Logf("Checkpoint IDs: %v", checkpointIDs)

	// All checkpoint IDs must be unique
	seen := make(map[string]bool)
	for i, cpID := range checkpointIDs {
		if seen[cpID] {
			t.Errorf("Duplicate checkpoint ID at position %d: %s", i, cpID)
		}
		seen[cpID] = true
	}

	// Verify TurnCheckpointIDs contains all 3
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if len(state.TurnCheckpointIDs) != 3 {
		t.Errorf("TurnCheckpointIDs should have 3 entries, got %d: %v",
			len(state.TurnCheckpointIDs), state.TurnCheckpointIDs)
	}

	// Add more work to transcript before stopping
	sess.TranscriptBuilder.AddAssistantMessage("All files created successfully!")
	if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Agent finishes - this should finalize ALL checkpoints
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify session is IDLE
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Expected IDLE phase, got %s", state.Phase)
	}

	// TurnCheckpointIDs should be cleared after finalization
	if len(state.TurnCheckpointIDs) != 0 {
		t.Errorf("TurnCheckpointIDs should be cleared, got %v", state.TurnCheckpointIDs)
	}

	// Verify all checkpoints exist and have finalized transcripts
	for i, cpID := range checkpointIDs {
		transcriptPath := SessionFilePath(cpID, paths.TranscriptFileName)
		content, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
		if !found {
			t.Errorf("Checkpoint %d transcript should exist at %s", i, transcriptPath)
			continue
		}
		// All transcripts should contain the final message
		if !strings.Contains(content, "All files created successfully") {
			t.Errorf("Checkpoint %d transcript should be finalized with final message", i)
		}
	}

	t.Log("MultipleCommits_SameActiveTurn test completed successfully")
}

// TestShadow_OverlapCheck_UnrelatedCommit tests that commits for files NOT
// touched by the session don't get checkpoint trailers (when session is not ACTIVE).
//
// Flow:
// 1. Agent touches file A, then stops (IDLE)
// 2. User commits file A → checkpoint (files overlap with session)
// 3. Session BaseCommit updated, FilesTouched cleared
// 4. User creates file B manually (not through session)
// 5. User commits file B → NO checkpoint (no overlap with session)
func TestShadow_OverlapCheck_UnrelatedCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create file A through session
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	sess.CreateTranscript("Create file A", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
	})

	// Stop session (becomes IDLE)
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit file A - should get checkpoint (overlaps with session)
	env.GitCommitWithShadowHooks("Add file A", "fileA.go")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have checkpoint trailer (files overlap)")
	}
	t.Logf("First checkpoint ID: %s", firstCheckpointID)

	// Create file B manually (not through session)
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")

	// Commit file B - should NOT get checkpoint (no overlap with session files)
	env.GitCommitWithShadowHooks("Add file B (manual)", "fileB.go")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)

	if secondCheckpointID != "" {
		t.Errorf("Second commit should NOT have checkpoint trailer "+
			"(file B not touched by session), got %s", secondCheckpointID)
	} else {
		t.Log("Second commit correctly has no checkpoint trailer (no overlap)")
	}

	t.Log("OverlapCheck_UnrelatedCommit test completed successfully")
}

// TestShadow_OverlapCheck_PartialOverlap tests that commits with SOME files
// from the session get checkpoint trailers, even if they include other files.
//
// Flow:
// 1. Agent touches file A, then stops (IDLE)
// 2. User creates file B manually
// 3. User commits both A and B → checkpoint (partial overlap is enough)
func TestShadow_OverlapCheck_PartialOverlap(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create file A through session
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	sess.CreateTranscript("Create file A", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
	})

	// Stop session (becomes IDLE)
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Create file B manually (not through session)
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")

	// Commit both files together - should get checkpoint (partial overlap is enough)
	env.GitCommitWithShadowHooks("Add files A and B", "fileA.go", "fileB.go")
	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)

	if checkpointID == "" {
		t.Error("Commit should have checkpoint trailer (file A overlaps with session)")
	} else {
		t.Logf("Checkpoint ID: %s (partial overlap triggered checkpoint)", checkpointID)
	}

	t.Log("OverlapCheck_PartialOverlap test completed successfully")
}

// TestShadow_SessionDepleted_ManualEditNoCheckpoint tests that once all session
// files are committed, subsequent manual edits (even to previously committed files)
// do NOT get checkpoint trailers.
//
// Flow:
// 1. Agent creates files A, B, C, then stops (IDLE)
// 2. User commits files A and B → checkpoint #1
// 3. User commits file C → checkpoint #2 (carry-forward if implemented, or just C)
// 4. Session is now "depleted" (all FilesTouched committed)
// 5. User manually edits file A and commits → NO checkpoint (session exhausted)
func TestShadow_SessionDepleted_ManualEditNoCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create 3 files through session
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")
	env.WriteFile("fileC.go", "package main\n\nfunc C() {}\n")
	sess.CreateTranscript("Create files A, B, and C", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
		{Path: "fileC.go", Content: "package main\n\nfunc C() {}\n"},
	})

	// Stop session (becomes IDLE)
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// First commit: files A and B
	env.GitCommitWithShadowHooks("Add files A and B", "fileA.go", "fileB.go")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have checkpoint trailer (files overlap with session)")
	}
	t.Logf("First checkpoint ID: %s", firstCheckpointID)

	// Second commit: file C
	env.GitCommitWithShadowHooks("Add file C", "fileC.go")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)
	// Note: Whether this gets a checkpoint depends on carry-forward implementation
	// for IDLE sessions. Log either way.
	if secondCheckpointID != "" {
		t.Logf("Second checkpoint ID: %s (carry-forward active for IDLE)", secondCheckpointID)
	} else {
		t.Log("Second commit has no checkpoint (IDLE sessions don't carry forward)")
	}

	// Verify session state - FilesTouched should be empty or session ended
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		// Session may have been cleaned up, which is fine
		t.Logf("Session state not found (may have been cleaned up): %v", err)
	} else {
		t.Logf("Session state after all commits: Phase=%s, FilesTouched=%v",
			state.Phase, state.FilesTouched)
	}

	// Now manually edit file A (which was already committed as part of session)
	env.WriteFile("fileA.go", "package main\n\n// Manual edit by user\nfunc A() { return }\n")

	// Commit the manual edit - should NOT get checkpoint
	env.GitCommitWithShadowHooks("Manual edit to file A", "fileA.go")
	thirdCommitHash := env.GetHeadHash()
	thirdCheckpointID := env.GetCheckpointIDFromCommitMessage(thirdCommitHash)

	if thirdCheckpointID != "" {
		t.Errorf("Third commit should NOT have checkpoint trailer "+
			"(manual edit after session depleted), got %s", thirdCheckpointID)
	} else {
		t.Log("Third commit correctly has no checkpoint trailer (session depleted)")
	}

	t.Log("SessionDepleted_ManualEditNoCheckpoint test completed successfully")
}

// TestShadow_RevertedFiles_ManualEditNoCheckpoint tests that after reverting
// uncommitted session files, manual edits with completely different content
// do NOT get checkpoint trailers.
//
// The overlap check is content-aware: it compares file hashes between the
// committed content and the shadow branch content. If they don't match,
// the file is not considered session-related.
//
// Flow:
// 1. Agent creates files A, B, C, then stops (IDLE)
// 2. User commits files A and B → checkpoint #1
// 3. User reverts file C (deletes it)
// 4. User manually creates file C with different content
// 5. User commits file C → NO checkpoint (content doesn't match shadow branch)
func TestShadow_RevertedFiles_ManualEditNoCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create 3 files through session
	env.WriteFile("fileA.go", "package main\n\nfunc A() {}\n")
	env.WriteFile("fileB.go", "package main\n\nfunc B() {}\n")
	env.WriteFile("fileC.go", "package main\n\nfunc C() {}\n")
	sess.CreateTranscript("Create files A, B, and C", []FileChange{
		{Path: "fileA.go", Content: "package main\n\nfunc A() {}\n"},
		{Path: "fileB.go", Content: "package main\n\nfunc B() {}\n"},
		{Path: "fileC.go", Content: "package main\n\nfunc C() {}\n"},
	})

	// Stop session (becomes IDLE)
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// First commit: files A and B
	env.GitCommitWithShadowHooks("Add files A and B", "fileA.go", "fileB.go")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have checkpoint trailer (files overlap with session)")
	}
	t.Logf("First checkpoint ID: %s", firstCheckpointID)

	// Revert file C (undo agent's changes)
	// Since fileC.go is a new file (untracked), we need to delete it
	if err := os.Remove(filepath.Join(env.RepoDir, "fileC.go")); err != nil {
		t.Fatalf("Failed to remove fileC.go: %v", err)
	}
	t.Log("Reverted fileC.go by removing it")

	// Verify file C is gone
	if _, err := os.Stat(filepath.Join(env.RepoDir, "fileC.go")); !os.IsNotExist(err) {
		t.Fatal("fileC.go should not exist after revert")
	}

	// User manually creates file C with DIFFERENT content (not what agent wrote)
	env.WriteFile("fileC.go", "package main\n\n// Completely different implementation\nfunc C() { panic(\"manual\") }\n")

	// Commit the manual file C - should NOT get checkpoint because content-aware
	// overlap check compares file hashes. The content is completely different
	// from what the session wrote, so it's not linked.
	env.GitCommitWithShadowHooks("Add file C (manual implementation)", "fileC.go")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)

	if secondCheckpointID != "" {
		t.Errorf("Second commit should NOT have checkpoint trailer "+
			"(content doesn't match shadow branch), got %s", secondCheckpointID)
	} else {
		t.Log("Second commit correctly has no checkpoint trailer (content mismatch)")
	}

	t.Log("RevertedFiles_ManualEditNoCheckpoint test completed successfully")
}
