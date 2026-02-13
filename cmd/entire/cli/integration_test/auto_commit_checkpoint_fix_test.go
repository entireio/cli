//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestDualStrategy_NoCheckpointForNoChanges verifies that the auto-commit strategy
// does NOT create a checkpoint when a prompt results in no file changes,
// even after a previous prompt that DID create file changes.
//
// This is the fix for ENT-70: auto-commit strategy was incorrectly triggering checkpoints
// because it parsed the entire transcript including file changes from previous prompts.
func TestDualStrategy_NoCheckpointForNoChanges(t *testing.T) {
	t.Parallel()

	// Only run for auto-commit strategy
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)

	// Create a session
	session := env.NewSession()

	// === FIRST PROMPT: Creates a file ===
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("First SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file (as if Claude Code wrote it)
	env.WriteFile("feature.go", "package feature\n\nfunc Hello() {}\n")

	// Create transcript for first prompt
	session.TranscriptBuilder.AddUserMessage("Create a hello function")
	session.TranscriptBuilder.AddAssistantMessage("I'll create that for you.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "feature.go", "package feature\n\nfunc Hello() {}\n")
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Get head hash before first stop
	hashBeforeFirstStop := env.GetHeadHash()

	// Simulate stop for first prompt
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("First SimulateStop failed: %v", err)
	}

	// Verify a commit was created (auto-commit creates commits on active branch)
	hashAfterFirstStop := env.GetHeadHash()
	if hashAfterFirstStop == hashBeforeFirstStop {
		t.Error("Expected commit to be created after first prompt with file changes")
	}

	// === SECOND PROMPT: No file changes ===
	err = env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("Second SimulateUserPromptSubmit failed: %v", err)
	}

	// Add second prompt to transcript (no file changes this time)
	session.TranscriptBuilder.AddUserMessage("What does the Hello function do?")
	session.TranscriptBuilder.AddAssistantMessage("The Hello function is currently empty. It doesn't do anything yet.")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Simulate stop for second prompt
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("Second SimulateStop failed: %v", err)
	}

	// Verify NO new commit was created (this is the bug fix!)
	hashAfterSecondStop := env.GetHeadHash()
	if hashAfterSecondStop != hashAfterFirstStop {
		t.Errorf("No commit should be created for prompt without file changes.\nHash after first stop: %s\nHash after second stop: %s",
			hashAfterFirstStop, hashAfterSecondStop)
	}

	// === THIRD PROMPT: Has file changes again ===
	err = env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("Third SimulateUserPromptSubmit failed: %v", err)
	}

	// Create another file
	env.WriteFile("feature2.go", "package feature\n\nfunc Goodbye() {}\n")

	// Add third prompt to transcript with file changes
	session.TranscriptBuilder.AddUserMessage("Add a Goodbye function")
	session.TranscriptBuilder.AddAssistantMessage("I'll add that.")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "feature2.go", "package feature\n\nfunc Goodbye() {}\n")
	session.TranscriptBuilder.AddToolResult(toolID2)
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Simulate stop for third prompt
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("Third SimulateStop failed: %v", err)
	}

	// Verify a commit WAS created for the third prompt
	hashAfterThirdStop := env.GetHeadHash()
	if hashAfterThirdStop == hashAfterSecondStop {
		t.Error("Expected commit to be created after third prompt with file changes")
	}
}

// TestDualStrategy_IncrementalPromptContent verifies that each checkpoint only
// includes prompts since the last checkpoint, not the entire session history.
//
// This is the auto-commit equivalent of the manual-commit incremental condensation test.
// For auto-commit strategy, each checkpoint creates a commit, so the prompt.txt should only
// contain prompts from that specific checkpoint, not previous ones.
func TestDualStrategy_IncrementalPromptContent(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)
	session := env.NewSession()

	// === FIRST PROMPT: Creates file A ===
	t.Log("Phase 1: First prompt creates file A")

	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("First SimulateUserPromptSubmit failed: %v", err)
	}

	fileAContent := "package main\n\nfunc FunctionA() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A for the first feature")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function A for you.")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)
	session.TranscriptBuilder.AddAssistantMessage("Done creating function A!")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("First SimulateStop failed: %v", err)
	}

	// Get checkpoint ID from first commit
	commit1Hash := env.GetHeadHash()
	checkpoint1ID := env.GetCheckpointIDFromCommitMessage(commit1Hash)
	t.Logf("First checkpoint: %s (commit %s)", checkpoint1ID, commit1Hash[:7])

	// Verify first checkpoint has prompt A (session files in numbered subdirectory)
	prompt1Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpoint1ID, "prompt.txt"))
	if !found {
		t.Fatal("First checkpoint should have prompt.txt on entire/checkpoints/v1 branch")
	}
	t.Logf("First checkpoint prompt.txt:\n%s", prompt1Content)

	if !strings.Contains(prompt1Content, "function A") {
		t.Error("First checkpoint prompt.txt should contain 'function A'")
	}

	// === SECOND PROMPT: Creates file B ===
	t.Log("Phase 2: Second prompt creates file B")

	err = env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("Second SimulateUserPromptSubmit failed: %v", err)
	}

	fileBContent := "package main\n\nfunc FunctionB() {}\n"
	env.WriteFile("b.go", fileBContent)

	session.TranscriptBuilder.AddUserMessage("Create function B for the second feature")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function B for you.")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID2)
	session.TranscriptBuilder.AddAssistantMessage("Done creating function B!")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("Second SimulateStop failed: %v", err)
	}

	// Get checkpoint ID from second commit
	commit2Hash := env.GetHeadHash()
	checkpoint2ID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Second checkpoint: %s (commit %s)", checkpoint2ID, commit2Hash[:7])

	if checkpoint1ID == checkpoint2ID {
		t.Error("Checkpoints should have different IDs")
	}

	// === VERIFY INCREMENTAL CONTENT ===
	t.Log("Phase 3: Verify second checkpoint only has prompt B (incremental)")

	// Session files are now in numbered subdirectory (e.g., 0/prompt.txt)
	prompt2Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpoint2ID, "prompt.txt"))
	if !found {
		t.Fatal("Second checkpoint should have prompt.txt on entire/checkpoints/v1 branch")
	}
	t.Logf("Second checkpoint prompt.txt:\n%s", prompt2Content)

	// Should contain prompt B
	if !strings.Contains(prompt2Content, "function B") {
		t.Error("Second checkpoint prompt.txt should contain 'function B'")
	}

	// Should NOT contain prompt A (already in first checkpoint)
	if strings.Contains(prompt2Content, "function A") {
		t.Error("Second checkpoint prompt.txt should NOT contain 'function A' (already in first checkpoint)")
	}

	t.Log("Incremental prompt content test completed successfully!")
}

// TestDualStrategy_SessionStateTracksTranscriptOffset verifies that session state
// correctly tracks the transcript offset (CheckpointTranscriptStart) across prompts.
// Note: cannot use t.Parallel() because we need t.Chdir to load session state.
func TestDualStrategy_SessionStateTracksTranscriptOffset(t *testing.T) {
	env := NewFeatureBranchEnv(t, strategy.StrategyNameAutoCommit)
	session := env.NewSession()

	// First prompt
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Session state is created by InitializeSession during UserPromptSubmit
	// We need to change to the repo directory to load session state (it uses GetGitCommonDir)
	t.Chdir(env.RepoDir)
	state, err := strategy.LoadSessionState(session.ID)
	if err != nil {
		t.Fatalf("LoadSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should have been created by InitializeSession")
	}
	if state.CheckpointTranscriptStart != 0 {
		t.Errorf("Initial CheckpointTranscriptStart should be 0, got %d", state.CheckpointTranscriptStart)
	}
	if state.StepCount != 0 {
		t.Errorf("Initial StepCount should be 0, got %d", state.StepCount)
	}

	// Create file and transcript
	env.WriteFile("test.go", "package test")
	session.CreateTranscript("Create test file", []FileChange{
		{Path: "test.go", Content: "package test"},
	})

	// Simulate stop - this should update CheckpointTranscriptStart
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify session state was updated with transcript position
	state, err = strategy.LoadSessionState(session.ID)
	if err != nil {
		t.Fatalf("LoadSessionState after stop failed: %v", err)
	}
	if state.CheckpointTranscriptStart == 0 {
		t.Error("CheckpointTranscriptStart should have been updated after checkpoint")
	}
	if state.StepCount != 1 {
		t.Errorf("StepCount should be 1, got %d", state.StepCount)
	}

	// Second prompt - add more to transcript
	err = env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("Second SimulateUserPromptSubmit failed: %v", err)
	}

	// Modify a file
	env.WriteFile("test.go", "package test\n\nfunc Test() {}\n")
	session.TranscriptBuilder.AddUserMessage("Add a test function")
	session.TranscriptBuilder.AddAssistantMessage("Adding test function.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "test.go", "package test\n\nfunc Test() {}\n")
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Simulate second stop
	err = env.SimulateStop(session.ID, session.TranscriptPath)
	if err != nil {
		t.Fatalf("Second SimulateStop failed: %v", err)
	}

	// Verify session state was updated again
	state, err = strategy.LoadSessionState(session.ID)
	if err != nil {
		t.Fatalf("LoadSessionState after second stop failed: %v", err)
	}
	if state.StepCount != 2 {
		t.Errorf("StepCount should be 2, got %d", state.StepCount)
	}
	// CheckpointTranscriptStart should be higher than after first stop
	t.Logf("Final CheckpointTranscriptStart: %d, StepCount: %d",
		state.CheckpointTranscriptStart, state.StepCount)
}
