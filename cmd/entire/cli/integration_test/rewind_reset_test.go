//go:build integration

package integration

import (
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/strategy"
)

// TestRewindReset_DeletesShadowBranch tests that rewind reset deletes the shadow branch.
func TestRewindReset_DeletesShadowBranch(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7]

	// Create a session and checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("test.txt", "content")
	session.CreateTranscript("Add test file", []FileChange{{Path: "test.txt", Content: "content"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify shadow branch exists
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("Shadow branch %s should exist before reset", shadowBranch)
	}
	t.Logf("Created shadow branch: %s", shadowBranch)

	// Run rewind reset
	output := env.RunCLI("rewind", "reset", "--force")
	t.Logf("Rewind reset output:\n%s", output)

	// Verify shadow branch is deleted
	if env.BranchExists(shadowBranch) {
		t.Errorf("Shadow branch %s should not exist after reset", shadowBranch)
	}

	// Verify output indicates deletion
	if !strings.Contains(output, "Deleted shadow branch") {
		t.Errorf("Expected 'Deleted shadow branch' in output, got: %s", output)
	}
}

// TestRewindReset_ClearsSessionState tests that rewind reset clears session state files.
func TestRewindReset_ClearsSessionState(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Create a session and checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("test.txt", "content")
	session.CreateTranscript("Add test file", []FileChange{{Path: "test.txt", Content: "content"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify session state files exist
	numStateFilesBefore, err := env.CountSessionStateFiles()
	if err != nil {
		t.Fatalf("Failed to count session state files: %v", err)
	}
	if numStateFilesBefore == 0 {
		t.Fatal("Expected session state files before reset")
	}

	// Run rewind reset
	output := env.RunCLI("rewind", "reset", "--force")
	t.Logf("Rewind reset output:\n%s", output)

	// Verify session state files are cleared
	numStateFilesAfter, err := env.CountSessionStateFiles()
	if err != nil {
		t.Fatalf("Failed to count session state files after reset: %v", err)
	}
	if numStateFilesAfter > 0 {
		t.Errorf("Expected no session state files after reset, got %d", numStateFilesAfter)
	}

	// Verify output indicates clearing
	if !strings.Contains(output, "Cleared session state") {
		t.Errorf("Expected 'Cleared session state' in output, got: %s", output)
	}

	t.Logf("Cleared %d session state files", numStateFilesBefore)
}

// TestRewindReset_NoShadowBranch tests reset when no shadow branch exists.
func TestRewindReset_NoShadowBranch(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Don't create any checkpoints, so no shadow branch exists

	// Run rewind reset - should handle gracefully
	output := env.RunCLI("rewind", "reset", "--force")
	t.Logf("Rewind reset output:\n%s", output)

	// Verify output indicates no shadow branch found
	if !strings.Contains(output, "No shadow branch found") {
		t.Errorf("Expected 'No shadow branch found' in output, got: %s", output)
	}
}

// TestRewindReset_WithMultipleSessions tests reset with multiple sessions.
func TestRewindReset_WithMultipleSessions(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()
	shadowBranch := "entire/" + baseHead[:7]

	// Create a session with multiple checkpoints (same session continues)
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session1) failed: %v", err)
	}

	env.WriteFile("test1.txt", "content1")
	session1.CreateTranscript("Add test1", []FileChange{{Path: "test1.txt", Content: "content1"}})
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Continue the same session with a second checkpoint
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (checkpoint 2) failed: %v", err)
	}

	env.WriteFile("test2.txt", "content2")
	session1.TranscriptBuilder = NewTranscriptBuilder()
	session1.CreateTranscript("Add test2", []FileChange{{Path: "test2.txt", Content: "content2"}})
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2) failed: %v", err)
	}

	// Verify session state file exists
	stateFileCount, err := env.CountSessionStateFiles()
	if err != nil {
		t.Fatalf("Failed to count session state files: %v", err)
	}
	if stateFileCount < 1 {
		t.Fatalf("Expected at least 1 session state file, got %d", stateFileCount)
	}

	// Run rewind reset
	output := env.RunCLI("rewind", "reset", "--force")
	t.Logf("Rewind reset output:\n%s", output)

	// Verify all session state files are cleared
	stateFileCount, err = env.CountSessionStateFiles()
	if err != nil {
		t.Fatalf("Failed to count session state files after reset: %v", err)
	}
	if stateFileCount > 0 {
		t.Errorf("Expected no session state files after reset, got %d", stateFileCount)
	}

	// Verify shadow branch is deleted
	if env.BranchExists(shadowBranch) {
		t.Errorf("Shadow branch %s should not exist after reset", shadowBranch)
	}

	// Verify session is mentioned in output
	if !strings.Contains(output, session1.ID) {
		t.Errorf("Expected session1 ID in output")
	}
}
