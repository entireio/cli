//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestShadow_TrailingTranscriptAppendedToCheckpoint tests that when an agent
// continues a conversation after a commit (condensation) without touching any
// new files, the trailing transcript is appended to the existing checkpoint.
//
// Scenario:
// 1. Start session, create a file, save checkpoint (SimulateStop)
// 2. User commits -> condensation creates checkpoint on entire/checkpoints/v1
// 3. Agent continues conversation (new prompt + response in transcript, no file edits)
// 4. SimulateStop -> HandleTurnEnd -> handleTrailingTranscript appends to checkpoint
// 5. Verify checkpoint transcript and prompts contain the trailing conversation
func TestShadow_TrailingTranscriptAppendedToCheckpoint(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/trailing-transcript")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// ========================================
	// Phase 1: Start session and create checkpoint
	// ========================================
	t.Log("Phase 1: Start session and create checkpoint")

	session := env.NewSession()
	// Pass transcript path so InitializeSession sets state.TranscriptPath
	// (needed later for trailing transcript detection)
	if err := env.SimulateUserPromptSubmitWithTranscript(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file (this counts as "files touched")
	fileContent := "package main\n\nfunc Hello() string {\n\treturn \"hello\"\n}\n"
	env.WriteFile("hello.go", fileContent)

	// Build transcript with file edit
	session.TranscriptBuilder.AddUserMessage("Create a hello function in hello.go")
	session.TranscriptBuilder.AddAssistantMessage("I'll create the hello function for you.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "hello.go", fileContent)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done! I created hello.go with the Hello() function.")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Save checkpoint (SimulateStop triggers SaveChanges -> creates shadow branch checkpoint)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify checkpoint exists
	rewindPoints := env.GetRewindPoints()
	if len(rewindPoints) != 1 {
		t.Fatalf("Expected 1 rewind point after first checkpoint, got %d", len(rewindPoints))
	}
	t.Logf("Checkpoint 1 created: %s", rewindPoints[0].Message)

	// ========================================
	// Phase 2: User commits -> condensation
	// ========================================
	t.Log("Phase 2: User commits - triggering condensation")

	env.GitCommitWithShadowHooks("Add hello function", "hello.go")

	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Fatal("Commit should have Entire-Checkpoint trailer after condensation")
	}
	t.Logf("Checkpoint ID from commit: %s", checkpointID)

	// Verify condensation happened - checkpoint exists on entire/checkpoints/v1
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist after condensation")
	}

	// Read the initial transcript from the checkpoint
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	initialTranscript, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Fatalf("Transcript should exist at %s after condensation", transcriptPath)
	}
	t.Logf("Initial transcript length: %d bytes", len(initialTranscript))

	// Read initial prompts
	promptPath := SessionFilePath(checkpointID, "prompt.txt")
	initialPrompts, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath)
	if !found {
		t.Fatalf("Prompts should exist at %s after condensation", promptPath)
	}
	t.Logf("Initial prompts:\n%s", initialPrompts)

	// Verify session state after condensation
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist after condensation")
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("LastCheckpointID should be set after condensation")
	}
	if state.StepCount != 0 {
		t.Errorf("StepCount should be 0 after condensation, got %d", state.StepCount)
	}
	t.Logf("Session state after condensation: LastCheckpointID=%s, StepCount=%d, CheckpointTranscriptStart=%d",
		state.LastCheckpointID, state.StepCount, state.CheckpointTranscriptStart)

	// ========================================
	// Phase 3: Agent continues with trailing conversation (no file edits)
	// ========================================
	t.Log("Phase 3: Agent continues conversation without file edits")

	// Add trailing conversation to transcript BEFORE the next UserPromptSubmit.
	// This simulates the agent explaining what it did after the commit,
	// which adds conversation to the transcript without any file edits.
	// The trailing content is already on disk when UserPromptSubmit fires.
	session.TranscriptBuilder.AddUserMessage("Can you explain what the Hello function does?")
	session.TranscriptBuilder.AddAssistantMessage("The Hello function returns the string \"hello\". It takes no parameters and returns a string type.")

	// Write the updated transcript (includes original + trailing content)
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write updated transcript: %v", err)
	}

	// ========================================
	// Phase 4: SimulateUserPromptSubmit triggers InitializeSession ->
	//          handleTrailingTranscript appends BEFORE clearing LastCheckpointID
	// ========================================
	t.Log("Phase 4: UserPromptSubmit - should append trailing transcript before clearing checkpoint ID")

	// Pass transcript path so InitializeSession can read the trailing content
	if err := env.SimulateUserPromptSubmitWithTranscript(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (trailing) failed: %v", err)
	}

	// ========================================
	// Phase 5: Verify trailing transcript was appended
	// ========================================
	t.Log("Phase 5: Verifying trailing transcript was appended to checkpoint")

	// Read the transcript again from the checkpoint
	updatedTranscript, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Fatalf("Transcript should still exist at %s after trailing append", transcriptPath)
	}

	// Transcript should be longer than before
	if len(updatedTranscript) <= len(initialTranscript) {
		t.Errorf("Transcript should be longer after trailing append: initial=%d, updated=%d",
			len(initialTranscript), len(updatedTranscript))
	} else {
		t.Logf("Transcript grew from %d to %d bytes", len(initialTranscript), len(updatedTranscript))
	}

	// The trailing conversation should be in the updated transcript
	if !strings.Contains(updatedTranscript, "explain what the Hello function does") {
		t.Error("Updated transcript should contain trailing user prompt")
	}

	// Read the prompts again
	updatedPrompts, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath)
	if !found {
		t.Fatalf("Prompts should still exist at %s after trailing append", promptPath)
	}

	// Prompts should include the trailing prompt
	if !strings.Contains(updatedPrompts, "explain what the Hello function does") {
		t.Error("Updated prompts should contain trailing user prompt")
	}
	t.Logf("Updated prompts:\n%s", updatedPrompts)

	// Verify session state was updated
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState (after trailing) failed: %v", err)
	}
	t.Logf("Session state after trailing: CheckpointTranscriptStart=%d", state.CheckpointTranscriptStart)

	t.Log("Trailing transcript test completed successfully!")
}

// TestShadow_TrailingTranscriptSkippedWhenNewFilesTouched tests that trailing
// transcript handling is skipped when the agent has created new checkpoints
// (i.e., touched new files) after condensation.
//
// This ensures trailing transcript only fires when StepCount == 0 (no new files
// touched since the last condensation).
func TestShadow_TrailingTranscriptSkippedWhenNewFilesTouched(t *testing.T) {
	t.Parallel()

	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/trailing-skip")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// ========================================
	// Phase 1: Start session, create checkpoint, commit
	// ========================================
	t.Log("Phase 1: Start session, create checkpoint, commit")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	fileContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileContent)
	session.CreateTranscript("Create function A", []FileChange{{Path: "a.go", Content: fileContent}})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit to trigger condensation
	env.GitCommitWithShadowHooks("Add function A", "a.go")

	checkpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if checkpointID == "" {
		t.Fatal("Commit should have checkpoint trailer")
	}

	// Read initial transcript
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	initialTranscript, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Fatalf("Initial transcript should exist")
	}

	// ========================================
	// Phase 2: Continue session WITH new file edits (StepCount > 0)
	// ========================================
	t.Log("Phase 2: Continue with new file edits")

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (continuing) failed: %v", err)
	}

	// Create a NEW file (this means SaveChanges will create a new checkpoint)
	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	// Reset transcript builder for the new turn
	session.TranscriptBuilder = NewTranscriptBuilder()
	session.CreateTranscript("Create function B", []FileChange{{Path: "b.go", Content: fileBContent}})

	// This Stop creates a NEW checkpoint (StepCount becomes > 0)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (with new files) failed: %v", err)
	}

	// ========================================
	// Phase 3: Verify trailing transcript was NOT appended
	// ========================================
	t.Log("Phase 3: Verify trailing transcript was not appended to old checkpoint")

	// The original checkpoint transcript should be unchanged
	unchangedTranscript, found := env.ReadFileFromBranch(paths.MetadataBranchName, transcriptPath)
	if !found {
		t.Fatalf("Transcript should still exist")
	}

	if unchangedTranscript != initialTranscript {
		t.Errorf("Checkpoint transcript should not change when new files are touched.\nInitial length: %d\nCurrent length: %d",
			len(initialTranscript), len(unchangedTranscript))
	} else {
		t.Log("Correctly skipped trailing transcript when new files were touched (StepCount > 0)")
	}

	t.Log("Trailing transcript skip test completed successfully!")
}
