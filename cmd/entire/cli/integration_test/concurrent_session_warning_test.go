//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"entire.io/cli/cmd/entire/cli/strategy"
)

// TestConcurrentSessionWarning_BlocksFirstPrompt verifies that when a user starts
// a new Claude session while another session has uncommitted changes (checkpoints),
// the first prompt is blocked with a continue:false JSON response.
func TestConcurrentSessionWarning_BlocksFirstPrompt(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start session A and create a checkpoint
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sessionA.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content from session A")
	sessionA.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content from session A"}})
	if err := env.SimulateStop(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (sessionA) failed: %v", err)
	}

	// Verify session A has checkpoints
	stateA, err := env.GetSessionState(sessionA.ID)
	if err != nil {
		t.Fatalf("GetSessionState (sessionA) failed: %v", err)
	}
	if stateA == nil {
		t.Fatal("Session A state should exist after Stop hook")
	}
	if stateA.CheckpointCount == 0 {
		t.Fatal("Session A should have at least 1 checkpoint")
	}
	t.Logf("Session A has %d checkpoint(s)", stateA.CheckpointCount)

	// Start session B - first prompt should be blocked
	sessionB := env.NewSession()
	output := env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// The hook should succeed (exit code 0) but output JSON with continue:false
	if output.Err != nil {
		t.Fatalf("Hook should succeed but output continue:false, got error: %v\nStderr: %s", output.Err, output.Stderr)
	}

	// Parse the JSON response
	var response struct {
		Continue   bool   `json:"continue"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(output.Stdout, &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v\nStdout: %s", err, output.Stdout)
	}

	// Verify continue is false
	if response.Continue {
		t.Error("Expected continue:false in JSON response")
	}

	// Verify stop reason contains expected message
	expectedMessage := "Another session is active"
	if !strings.Contains(response.StopReason, expectedMessage) {
		t.Errorf("StopReason should contain %q, got: %s", expectedMessage, response.StopReason)
	}

	t.Logf("Received expected blocking response: %s", output.Stdout)
}

// TestConcurrentSessionWarning_SetsWarningFlag verifies that after the first prompt
// is blocked, the session state has ConcurrentWarningShown set to true.
func TestConcurrentSessionWarning_SetsWarningFlag(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start session A and create a checkpoint
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sessionA.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	sessionA.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateStop(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (sessionA) failed: %v", err)
	}

	// Start session B - first prompt is blocked
	sessionB := env.NewSession()
	_ = env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// Verify session B state has ConcurrentWarningShown flag
	stateB, err := env.GetSessionState(sessionB.ID)
	if err != nil {
		t.Fatalf("GetSessionState (sessionB) failed: %v", err)
	}
	if stateB == nil {
		t.Fatal("Session B state should exist after blocked prompt")
	}
	if !stateB.ConcurrentWarningShown {
		t.Error("Session B state should have ConcurrentWarningShown=true")
	}

	t.Logf("Session B state: ConcurrentWarningShown=%v", stateB.ConcurrentWarningShown)
}

// TestConcurrentSessionWarning_SubsequentPromptsSucceed verifies that after the
// warning is shown, subsequent prompts in the same session proceed normally.
func TestConcurrentSessionWarning_SubsequentPromptsSucceed(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start session A and create a checkpoint
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sessionA.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	sessionA.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateStop(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (sessionA) failed: %v", err)
	}

	// Start session B - first prompt is blocked
	sessionB := env.NewSession()
	output1 := env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// Verify first prompt was blocked
	var response1 struct {
		Continue bool `json:"continue"`
	}
	if err := json.Unmarshal(output1.Stdout, &response1); err != nil {
		t.Fatalf("Failed to parse first response: %v", err)
	}
	if response1.Continue {
		t.Fatal("First prompt should have been blocked")
	}
	t.Log("First prompt correctly blocked")

	// Second prompt in session B should PROCEED normally (both sessions capture checkpoints)
	// The warning was shown on first prompt, but subsequent prompts continue to capture state
	output2 := env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// The hook should succeed
	if output2.Err != nil {
		t.Errorf("Second prompt should succeed, got error: %v", output2.Err)
	}

	// The hook should process normally (capture state)
	// Output should contain state capture info, not a blocking response
	if len(output2.Stdout) > 0 {
		// Check if it's a blocking JSON response (which it shouldn't be anymore after the first prompt)
		var blockResponse struct {
			Continue bool `json:"continue"`
		}
		if json.Unmarshal(output2.Stdout, &blockResponse) == nil && !blockResponse.Continue {
			t.Errorf("Second prompt should not be blocked after warning was shown, got: %s", output2.Stdout)
		}
	}

	// Warning flag should remain set (for tracking)
	stateB, _ := env.GetSessionState(sessionB.ID)
	if stateB == nil {
		t.Fatal("Session B state should exist")
	}
	if !stateB.ConcurrentWarningShown {
		t.Error("ConcurrentWarningShown should remain true after second prompt")
	}

	t.Log("Second prompt correctly processed (both sessions capture checkpoints)")
}

// TestConcurrentSessionWarning_NoWarningWithoutCheckpoints verifies that starting
// a new session does NOT trigger the warning if the existing session has no checkpoints.
func TestConcurrentSessionWarning_NoWarningWithoutCheckpoints(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start session A but do NOT create any checkpoints
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sessionA.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionA) failed: %v", err)
	}

	// Verify session A has no checkpoints
	stateA, err := env.GetSessionState(sessionA.ID)
	if err != nil {
		t.Fatalf("GetSessionState (sessionA) failed: %v", err)
	}
	if stateA == nil {
		t.Fatal("Session A state should exist after UserPromptSubmit")
	}
	if stateA.CheckpointCount != 0 {
		t.Fatalf("Session A should have 0 checkpoints, got %d", stateA.CheckpointCount)
	}

	// Start session B - should NOT be blocked since session A has no checkpoints
	sessionB := env.NewSession()
	output := env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// Check if we got a blocking response
	if len(output.Stdout) > 0 {
		var response struct {
			Continue   bool   `json:"continue"`
			StopReason string `json:"stopReason,omitempty"`
		}
		if json.Unmarshal(output.Stdout, &response) == nil {
			if !response.Continue && strings.Contains(response.StopReason, "another active session") {
				t.Error("Should NOT show concurrent session warning when existing session has no checkpoints")
			}
		}
	}

	// Session B should proceed normally (or fail for other reasons, but not concurrent warning)
	stateB, _ := env.GetSessionState(sessionB.ID)
	if stateB != nil && stateB.ConcurrentWarningShown {
		t.Error("Session B should not have ConcurrentWarningShown set when session A has no checkpoints")
	}

	t.Log("No concurrent session warning shown when existing session has no checkpoints")
}

// TestConcurrentSessions_BothCondensedOnCommit verifies that when two sessions have
// interleaved checkpoints, committing preserves both sessions' logs on entire/sessions.
func TestConcurrentSessions_BothCondensedOnCommit(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Session A: create checkpoint
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sessionA.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionA) failed: %v", err)
	}

	env.WriteFile("fileA.txt", "content from session A")
	sessionA.CreateTranscript("Add file A", []FileChange{{Path: "fileA.txt", Content: "content from session A"}})
	if err := env.SimulateStop(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (sessionA) failed: %v", err)
	}

	// Session B: acknowledge warning and create checkpoint
	sessionB := env.NewSession()
	// First prompt is blocked with warning
	_ = env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// Second prompt proceeds (after warning was shown)
	if err := env.SimulateUserPromptSubmit(sessionB.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionB second prompt) failed: %v", err)
	}

	env.WriteFile("fileB.txt", "content from session B")
	sessionB.CreateTranscript("Add file B", []FileChange{{Path: "fileB.txt", Content: "content from session B"}})
	if err := env.SimulateStop(sessionB.ID, sessionB.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (sessionB) failed: %v", err)
	}

	// Verify both sessions have checkpoints
	stateA, _ := env.GetSessionState(sessionA.ID)
	stateB, _ := env.GetSessionState(sessionB.ID)
	if stateA == nil || stateA.CheckpointCount == 0 {
		t.Fatal("Session A should have checkpoints")
	}
	if stateB == nil || stateB.CheckpointCount == 0 {
		t.Fatal("Session B should have checkpoints")
	}
	t.Logf("Session A: %d checkpoints, Session B: %d checkpoints", stateA.CheckpointCount, stateB.CheckpointCount)

	// Commit with hooks - this should condense both sessions
	env.GitCommitWithShadowHooks("Add files from both sessions", "fileA.txt", "fileB.txt")

	// Get the checkpoint ID from entire/sessions
	checkpointID := env.GetLatestCheckpointID()
	if checkpointID == "" {
		t.Fatal("Failed to get checkpoint ID from entire/sessions branch")
	}
	t.Logf("Checkpoint ID: %s", checkpointID)

	// Build the sharded path
	shardedPath := checkpointID[:2] + "/" + checkpointID[2:]

	// Verify metadata.json exists and has multi-session info
	metadataContent, found := env.ReadFileFromBranch("entire/sessions", shardedPath+"/metadata.json")
	if !found {
		t.Fatal("metadata.json should exist on entire/sessions branch")
	}

	var metadata struct {
		SessionCount int      `json:"session_count"`
		SessionIDs   []string `json:"session_ids"`
		SessionID    string   `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(metadataContent), &metadata); err != nil {
		t.Fatalf("Failed to parse metadata.json: %v", err)
	}

	t.Logf("Metadata: session_count=%d, session_ids=%v, session_id=%s",
		metadata.SessionCount, metadata.SessionIDs, metadata.SessionID)

	// Verify multi-session fields
	if metadata.SessionCount != 2 {
		t.Errorf("Expected session_count=2, got %d", metadata.SessionCount)
	}
	if len(metadata.SessionIDs) != 2 {
		t.Errorf("Expected 2 session_ids, got %d", len(metadata.SessionIDs))
	}

	// Verify session_id points to the latest session (session B)
	// Session IDs are prefixed with today's date in YYYY-MM-DD format
	expectedSessionID := time.Now().Format("2006-01-02") + "-" + sessionB.ID
	if metadata.SessionID != expectedSessionID {
		t.Errorf("Expected session_id=%s (latest session), got %s", expectedSessionID, metadata.SessionID)
	}

	// Verify archived session exists in subfolder "1/"
	archivedMetadata, found := env.ReadFileFromBranch("entire/sessions", shardedPath+"/1/metadata.json")
	if !found {
		t.Error("Archived session metadata should exist at 1/metadata.json")
	} else {
		t.Logf("Archived session metadata found: %s", archivedMetadata[:min(100, len(archivedMetadata))])
	}

	// Verify transcript exists for current session (at root)
	if !env.FileExistsInBranch("entire/sessions", shardedPath+"/full.jsonl") {
		t.Error("Current session transcript should exist at root (full.jsonl)")
	}

	// Verify transcript exists for archived session
	if !env.FileExistsInBranch("entire/sessions", shardedPath+"/1/full.jsonl") {
		t.Error("Archived session transcript should exist at 1/full.jsonl")
	}

	t.Log("Both sessions successfully condensed with proper archiving")
}
