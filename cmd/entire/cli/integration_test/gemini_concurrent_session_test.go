//go:build integration

package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// TestGeminiConcurrentSessionWarning_BlocksFirstPrompt verifies that when a user starts
// a new Gemini session while another session has uncommitted changes (checkpoints),
// the first prompt is blocked with a Gemini-format JSON response (decision: block).
func TestGeminiConcurrentSessionWarning_BlocksFirstPrompt(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, "gemini")

	// Start session A and create a checkpoint
	sessionA := env.NewGeminiSession()
	if err := env.SimulateGeminiBeforeAgent(sessionA.ID); err != nil {
		t.Fatalf("SimulateGeminiBeforeAgent (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content from session A")
	sessionA.CreateGeminiTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content from session A"}})
	if err := env.SimulateGeminiSessionEnd(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateGeminiSessionEnd (sessionA) failed: %v", err)
	}

	// Verify session A has checkpoints
	stateA, err := env.GetSessionState(sessionA.ID)
	if err != nil {
		t.Fatalf("GetSessionState (sessionA) failed: %v", err)
	}
	if stateA == nil {
		t.Fatal("Session A state should exist after SessionEnd hook")
	}
	if stateA.CheckpointCount == 0 {
		t.Fatal("Session A should have at least 1 checkpoint")
	}
	t.Logf("Session A has %d checkpoint(s)", stateA.CheckpointCount)

	// Start session B - first prompt should be blocked
	sessionB := env.NewGeminiSession()
	output := env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

	// Gemini blocking exits with code 0 (JSON parsed) and decision "block"
	if output.Err != nil {
		t.Fatalf("Hook should exit with code 0 for blocking, got error: %v", output.Err)
	}

	// Parse the JSON response (Gemini format)
	var response struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(output.Stdout, &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v\nStdout: %s", err, output.Stdout)
	}

	// Verify decision is block
	if response.Decision != "block" {
		t.Errorf("Expected decision:block in JSON response, got: %s", response.Decision)
	}

	// Verify reason contains expected message
	expectedMessage := "another active session with uncommitted changes"
	if !strings.Contains(response.Reason, expectedMessage) {
		t.Errorf("Reason should contain %q, got: %s", expectedMessage, response.Reason)
	}

	// Verify the resume command mentions gemini --resume
	expectedResumeCmd := "gemini --resume"
	if !strings.Contains(response.Reason, expectedResumeCmd) {
		t.Errorf("Reason should contain %q, got: %s", expectedResumeCmd, response.Reason)
	}

	t.Logf("Received expected blocking response: %s", output.Stdout)
}

// TestGeminiConcurrentSessionWarning_SetsWarningFlag verifies that after the first prompt
// is blocked, the session state has ConcurrentWarningShown set to true.
func TestGeminiConcurrentSessionWarning_SetsWarningFlag(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, "gemini")

	// Start session A and create a checkpoint
	sessionA := env.NewGeminiSession()
	if err := env.SimulateGeminiBeforeAgent(sessionA.ID); err != nil {
		t.Fatalf("SimulateGeminiBeforeAgent (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	sessionA.CreateGeminiTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateGeminiSessionEnd(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateGeminiSessionEnd (sessionA) failed: %v", err)
	}

	// Start session B - first prompt is blocked
	sessionB := env.NewGeminiSession()
	_ = env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

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

// TestGeminiConcurrentSessionWarning_SubsequentPromptsSucceed verifies that after the
// warning is shown, subsequent prompts in the same session are skipped silently.
func TestGeminiConcurrentSessionWarning_SubsequentPromptsSucceed(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, "gemini")

	// Start session A and create a checkpoint
	sessionA := env.NewGeminiSession()
	if err := env.SimulateGeminiBeforeAgent(sessionA.ID); err != nil {
		t.Fatalf("SimulateGeminiBeforeAgent (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	sessionA.CreateGeminiTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateGeminiSessionEnd(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateGeminiSessionEnd (sessionA) failed: %v", err)
	}

	// Start session B - first prompt is blocked (exits with code 0, decision: block)
	sessionB := env.NewGeminiSession()
	output1 := env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

	// Verify first prompt was blocked (exit code 0 with decision: block)
	if output1.Err != nil {
		t.Fatalf("First prompt should exit with code 0, got error: %v", output1.Err)
	}

	var response1 struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(output1.Stdout, &response1); err != nil {
		t.Fatalf("Failed to parse first response: %v", err)
	}
	if response1.Decision != "block" {
		t.Fatalf("First prompt should have decision:block, got: %s", response1.Decision)
	}
	t.Log("First prompt correctly blocked")

	// Second prompt in session B should be skipped entirely (no processing)
	// Since ConcurrentWarningShown is true, the hook returns nil and produces no output
	output2 := env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

	// The hook should succeed (no error) because it skips silently
	if output2.Err != nil {
		t.Errorf("Second prompt should succeed (skip silently), got error: %v", output2.Err)
	}

	// The hook should produce no output (it was skipped)
	if len(output2.Stdout) > 0 {
		t.Errorf("Second prompt should produce no output (hook skipped), got: %s", output2.Stdout)
	}

	// The important assertion: warning flag should still be set
	stateB, _ := env.GetSessionState(sessionB.ID)
	if stateB == nil {
		t.Fatal("Session B state should exist")
	}
	if !stateB.ConcurrentWarningShown {
		t.Error("ConcurrentWarningShown should remain true after second prompt")
	}

	t.Log("Second prompt correctly skipped (hooks disabled for warned session)")
}

// TestGeminiConcurrentSessionWarning_NoWarningWithoutCheckpoints verifies that starting
// a new session does NOT trigger the warning if the existing session has no checkpoints.
func TestGeminiConcurrentSessionWarning_NoWarningWithoutCheckpoints(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, "gemini")

	// Start session A but do NOT create any checkpoints
	sessionA := env.NewGeminiSession()
	if err := env.SimulateGeminiBeforeAgent(sessionA.ID); err != nil {
		t.Fatalf("SimulateGeminiBeforeAgent (sessionA) failed: %v", err)
	}

	// Verify session A has no checkpoints
	stateA, err := env.GetSessionState(sessionA.ID)
	if err != nil {
		t.Fatalf("GetSessionState (sessionA) failed: %v", err)
	}
	if stateA == nil {
		t.Fatal("Session A state should exist after BeforeAgent hook")
	}
	if stateA.CheckpointCount != 0 {
		t.Fatalf("Session A should have 0 checkpoints, got %d", stateA.CheckpointCount)
	}

	// Start session B - should NOT be blocked since session A has no checkpoints
	sessionB := env.NewGeminiSession()
	output := env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

	// Check if we got a blocking response (we shouldn't)
	// With exit code 0, check if there's a blocking decision in the JSON
	if len(output.Stdout) > 0 {
		var response struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason,omitempty"`
		}
		if json.Unmarshal(output.Stdout, &response) == nil {
			if response.Decision == "block" && strings.Contains(response.Reason, "another active session") {
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

// TestGeminiConcurrentSessionWarning_ResumeCommandFormat verifies that the blocking
// message includes the correct Gemini resume command format.
func TestGeminiConcurrentSessionWarning_ResumeCommandFormat(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, "gemini")

	// Start session A and create a checkpoint
	sessionA := env.NewGeminiSession()
	if err := env.SimulateGeminiBeforeAgent(sessionA.ID); err != nil {
		t.Fatalf("SimulateGeminiBeforeAgent (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	sessionA.CreateGeminiTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateGeminiSessionEnd(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateGeminiSessionEnd (sessionA) failed: %v", err)
	}

	// Start session B - triggers blocking
	sessionB := env.NewGeminiSession()
	output := env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

	// Parse the blocking response
	var response struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(output.Stdout, &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v\nStdout: %s", err, output.Stdout)
	}

	// Verify the resume command is for Gemini CLI, not Claude
	if !strings.Contains(response.Reason, "gemini --resume") {
		t.Errorf("Reason should contain 'gemini --resume', got: %s", response.Reason)
	}
	if strings.Contains(response.Reason, "claude -r") {
		t.Errorf("Reason should NOT contain Claude's resume command, got: %s", response.Reason)
	}
	if !strings.Contains(response.Reason, "close Gemini CLI") {
		t.Errorf("Reason should mention closing Gemini CLI, got: %s", response.Reason)
	}

	t.Logf("Resume command correctly formatted for Gemini CLI: %s", response.Reason)
}

// TestCrossAgentConcurrentSession_ClaudeSessionShowsClaudeResumeInGemini verifies that
// when a Claude Code session exists with checkpoints and Gemini tries to start,
// the blocking message shows "claude -r" (the Claude resume command), not "gemini --resume".
func TestCrossAgentConcurrentSession_ClaudeSessionShowsClaudeResumeInGemini(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	// Initialize with Claude Code agent first
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, agent.AgentNameClaudeCode)

	// Start Claude session A and create a checkpoint
	sessionA := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sessionA.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content from Claude session")
	sessionA.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content from Claude session"}})
	if err := env.SimulateStop(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (sessionA) failed: %v", err)
	}

	// Verify Claude session A has checkpoints and correct agent type
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
	if stateA.AgentType != "Claude Code" {
		t.Errorf("Session A agent type should be 'Claude Code', got: %s", stateA.AgentType)
	}
	t.Logf("Claude session A has %d checkpoint(s), agent type: %s", stateA.CheckpointCount, stateA.AgentType)

	// Now try to start a Gemini session - should be blocked with Claude resume command
	sessionB := env.NewGeminiSession()
	output := env.SimulateGeminiBeforeAgentWithOutput(sessionB.ID)

	// Parse the JSON response (Gemini format)
	var response struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(output.Stdout, &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v\nStdout: %s", err, output.Stdout)
	}

	// Verify decision is block
	if response.Decision != "block" {
		t.Errorf("Expected decision:block in JSON response, got: %s", response.Decision)
	}

	// CRITICAL: The resume command should be for Claude Code (the conflicting session's agent),
	// NOT Gemini (the current agent).
	if !strings.Contains(response.Reason, "claude -r") {
		t.Errorf("Resume command should be 'claude -r' (Claude session is conflicting), got: %s", response.Reason)
	}
	if strings.Contains(response.Reason, "gemini --resume") {
		t.Errorf("Resume command should NOT be 'gemini --resume' (that's the current agent, not conflicting session), got: %s", response.Reason)
	}

	// Extract the session ID from the resume command
	expectedSessionID := sessionA.ID[:len(sessionA.ID)-11] // Remove date prefix for raw session ID
	if !strings.Contains(response.Reason, expectedSessionID) {
		t.Errorf("Resume command should contain session ID %q, got: %s", expectedSessionID, response.Reason)
	}

	t.Logf("Cross-agent blocking correctly shows Claude resume command: %s", response.Reason)
}

// TestCrossAgentConcurrentSession_GeminiSessionShowsGeminiResumeInClaude verifies that
// when a Gemini CLI session exists with checkpoints and Claude tries to start,
// the blocking message shows "gemini --resume" (the Gemini resume command), not "claude -r".
func TestCrossAgentConcurrentSession_GeminiSessionShowsGeminiResumeInClaude(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	// Initialize with Gemini agent first
	env.InitEntireWithAgent(strategy.StrategyNameManualCommit, agent.AgentNameGemini)

	// Start Gemini session A and create a checkpoint
	sessionA := env.NewGeminiSession()
	if err := env.SimulateGeminiBeforeAgent(sessionA.ID); err != nil {
		t.Fatalf("SimulateGeminiBeforeAgent (sessionA) failed: %v", err)
	}

	env.WriteFile("file.txt", "content from Gemini session")
	sessionA.CreateGeminiTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content from Gemini session"}})
	if err := env.SimulateGeminiSessionEnd(sessionA.ID, sessionA.TranscriptPath); err != nil {
		t.Fatalf("SimulateGeminiSessionEnd (sessionA) failed: %v", err)
	}

	// Verify Gemini session A has checkpoints and correct agent type
	stateA, err := env.GetSessionState(sessionA.ID)
	if err != nil {
		t.Fatalf("GetSessionState (sessionA) failed: %v", err)
	}
	if stateA == nil {
		t.Fatal("Session A state should exist after SessionEnd hook")
	}
	if stateA.CheckpointCount == 0 {
		t.Fatal("Session A should have at least 1 checkpoint")
	}
	if stateA.AgentType != "Gemini CLI" {
		t.Errorf("Session A agent type should be 'Gemini CLI', got: %s", stateA.AgentType)
	}
	t.Logf("Gemini session A has %d checkpoint(s), agent type: %s", stateA.CheckpointCount, stateA.AgentType)

	// Now try to start a Claude session - should be blocked with Gemini resume command
	sessionB := env.NewSession()
	output := env.SimulateUserPromptSubmitWithOutput(sessionB.ID)

	// Parse the JSON response (Claude format)
	var response struct {
		Continue   bool   `json:"continue"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(output.Stdout, &response); err != nil {
		t.Fatalf("Failed to parse JSON response: %v\nStdout: %s", err, output.Stdout)
	}

	// Verify continue is false (blocked)
	if response.Continue {
		t.Error("Expected continue:false in JSON response (blocked)")
	}

	// CRITICAL: The resume command should be for Gemini CLI (the conflicting session's agent),
	// NOT Claude (the current agent).
	if !strings.Contains(response.StopReason, "gemini --resume") {
		t.Errorf("Resume command should be 'gemini --resume' (Gemini session is conflicting), got: %s", response.StopReason)
	}
	if strings.Contains(response.StopReason, "claude -r") {
		t.Errorf("Resume command should NOT be 'claude -r' (that's the current agent, not conflicting session), got: %s", response.StopReason)
	}

	// Extract the session ID from the resume command
	expectedSessionID := sessionA.ID[:len(sessionA.ID)-11] // Remove date prefix for raw session ID
	if !strings.Contains(response.StopReason, expectedSessionID) {
		t.Errorf("Resume command should contain session ID %q, got: %s", expectedSessionID, response.StopReason)
	}

	t.Logf("Cross-agent blocking correctly shows Gemini resume command: %s", response.StopReason)
}
