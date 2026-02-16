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

// TestShadowStrategy_MidSessionCommit_FromTranscript tests that when Claude commits
// mid-session (before Stop has been called), the prepare-commit-msg hook detects
// the new work by checking the live transcript and adds a checkpoint trailer.
//
// This is scenario 2 from ENT-112:
// - User prompts Claude
// - Claude creates files and commits them
// - No Stop has happened yet (no shadow branch)
// - The commit should still get a checkpoint trailer because the transcript shows file modifications
func TestShadowStrategy_MidSessionCommit_FromTranscript(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	// Simulate user prompt (initializes session with BaseCommit and TranscriptPath)
	// We need to pass the transcript path so it gets stored in session state
	input := map[string]string{
		"session_id":      session.ID,
		"transcript_path": session.TranscriptPath,
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

	// Verify session state has transcript path
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("Session state is nil")
	}
	if state.TranscriptPath == "" {
		t.Error("Session state should have TranscriptPath after user-prompt-submit")
	}
	t.Logf("Session state TranscriptPath: %s", state.TranscriptPath)

	// Create a file as if Claude wrote it
	env.WriteFile("claude_file.txt", "content from Claude")

	// Create transcript showing Claude created the file (NO Stop called)
	session.CreateTranscript("Create a file for me", []FileChange{
		{Path: "claude_file.txt", Content: "content from Claude"},
	})

	// Verify NO shadow branch exists (Stop hasn't been called)
	shadowBranches := env.ListBranchesWithPrefix("entire/")
	hasShadowBranch := false
	for _, b := range shadowBranches {
		if b != paths.MetadataBranchName {
			hasShadowBranch = true
			break
		}
	}
	if hasShadowBranch {
		t.Error("Shadow branch should not exist before Stop is called")
	}

	// Get HEAD before commit
	headBefore := env.GetHeadHash()

	// Commit with shadow hooks - should add trailer because transcript shows file modifications
	env.GitCommitWithShadowHooks("Add file from Claude (mid-session)", "claude_file.txt")

	// Get the commit
	commitHash := env.GetHeadHash()
	if commitHash == headBefore {
		t.Fatal("Commit was not created")
	}

	// CRITICAL: Verify commit has a checkpoint trailer
	// This is the fix for ENT-112 scenario 2: detect work from live transcript
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Error("Mid-session commit should have Entire-Checkpoint trailer when transcript shows file modifications")
	} else {
		t.Logf("Mid-session commit has checkpoint ID: %s", checkpointID)
	}
}

// TestShadowStrategy_MidSessionCommit_NoTrailerWithoutTranscriptPath tests that
// when TranscriptPath is not set in session state, commits don't get erroneous
// checkpoint trailers (graceful fallback).
func TestShadowStrategy_MidSessionCommit_NoTrailerWithoutTranscriptPath(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	// Simulate user prompt WITHOUT transcript path (legacy behavior)
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file manually (not through Claude)
	env.WriteFile("manual_file.txt", "manual content")

	// Don't create transcript - simulating a case where transcript path isn't available

	// Commit with shadow hooks
	env.GitCommitWithShadowHooks("Manual commit without transcript", "manual_file.txt")

	// Commit should NOT have checkpoint trailer (no session activity detected)
	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID != "" {
		t.Errorf("Commit without session activity should not have checkpoint trailer, got: %s", checkpointID)
	}
}

// TestShadowStrategy_MidSessionCommit_NoTrailerForUnrelatedFile tests that
// when Claude has modified files but the committed file is unrelated,
// no checkpoint trailer is added.
func TestShadowStrategy_MidSessionCommit_NoTrailerForUnrelatedFile(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	// Simulate user prompt with transcript path
	input := map[string]string{
		"session_id":      session.ID,
		"transcript_path": session.TranscriptPath,
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

	// Create transcript showing Claude modified a DIFFERENT file
	session.CreateTranscript("Create a file", []FileChange{
		{Path: "claude_file.txt", Content: "content from Claude"},
	})

	// Create and commit an UNRELATED file (not in transcript)
	env.WriteFile("unrelated_file.txt", "unrelated content")

	// Commit with shadow hooks - should NOT add trailer because files don't overlap
	env.GitCommitWithShadowHooks("Unrelated file commit", "unrelated_file.txt")

	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)

	// CRITICAL: No checkpoint trailer should be added for unrelated files
	// The overlap check in sessionHasNewContentFromLiveTranscript ensures this
	if checkpointID != "" {
		t.Errorf("Unrelated file commit should NOT have checkpoint trailer, but got: %s", checkpointID)
	} else {
		t.Log("Correctly omitted checkpoint trailer for unrelated file commit")
	}
}

// TestShadowStrategy_AgentCommit_AlwaysGetsTrailer tests that when an agent commits
// (ACTIVE session + no TTY), the trailer is always added regardless of content
// detection. This is the fast path that bypasses transcript analysis.
func TestShadowStrategy_AgentCommit_AlwaysGetsTrailer(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	// Start session (sets phase to ACTIVE)
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file and commit as agent (no TTY)
	env.WriteFile("agent_file.txt", "created by agent")
	env.GitCommitWithShadowHooksAsAgent("Agent commit", "agent_file.txt")

	// Agent commits should ALWAYS get a trailer (fast path, no content detection)
	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Error("Agent commit during ACTIVE session should always get a checkpoint trailer")
	} else {
		t.Logf("Agent commit correctly got checkpoint trailer: %s", checkpointID)
	}
}

// TestAgentCommitMidTurn_CondensingWithoutSaveChanges tests that when an agent
// commits mid-turn (before Stop/SaveChanges runs), the PostCommit hook still
// condenses the session data to entire/checkpoints/v1.
//
// This is the fix for #274: previously, StepCount=0 and FilesTouched=[] caused
// hasNew=false, so no condensation occurred. Now we check TranscriptPath != ""
// as a signal that the session is properly initialized and has work to condense.
//
// Flow:
//  1. Start session (ACTIVE phase, TranscriptPath set)
//  2. Agent creates files, writes transcript (NO SaveChanges/Stop)
//  3. Agent commits mid-turn (no shadow branch exists)
//  4. Verify: checkpoint trailer added to commit
//  5. Verify: condensation happened — checkpoint on entire/checkpoints/v1
//  6. Verify: session stays ACTIVE
//  7. Agent stops → session transitions to IDLE cleanly
func TestAgentCommitMidTurn_CondensingWithoutSaveChanges(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session with transcript path (needed for mid-turn commit detection)
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("user-prompt-submit failed: %v", err)
	}

	// Verify session is ACTIVE with TranscriptPath
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state is nil")
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase should be %q, got %q", session.PhaseActive, state.Phase)
	}
	if state.TranscriptPath == "" {
		t.Fatal("Session state should have TranscriptPath")
	}

	// Verify NO shadow branch exists (Stop/SaveChanges never called)
	shadowBranches := env.ListBranchesWithPrefix("entire/")
	for _, b := range shadowBranches {
		if b != paths.MetadataBranchName {
			t.Fatalf("Shadow branch %s should not exist before Stop", b)
		}
	}

	// Agent creates a file and writes transcript (simulating Claude working)
	env.WriteFile("agent_feature.go", "package main\n\nfunc AgentFeature() {}\n")
	sess.CreateTranscript("Implement the agent feature", []FileChange{
		{Path: "agent_feature.go", Content: "package main\n\nfunc AgentFeature() {}\n"},
	})

	// Agent commits mid-turn (as agent, no TTY) — NO Stop/SaveChanges has run
	headBefore := env.GetHeadHash()
	env.GitCommitWithShadowHooksAsAgent("Implement agent feature", "agent_feature.go")

	commitHash := env.GetHeadHash()
	if commitHash == headBefore {
		t.Fatal("Commit was not created")
	}

	// Step 4: Verify checkpoint trailer was added
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Fatal("Agent mid-turn commit should have Entire-Checkpoint trailer")
	}
	t.Logf("Agent mid-turn commit has checkpoint ID: %s", checkpointID)

	// Step 5: CRITICAL — Verify condensation actually happened
	// This is the core assertion for #274: the checkpoint must exist on entire/checkpoints/v1
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist after mid-turn commit condensation")
	}
	summaryPath := CheckpointSummaryPath(checkpointID)
	if !env.FileExistsInBranch(paths.MetadataBranchName, summaryPath) {
		t.Fatalf("Checkpoint metadata should exist at %s on entire/checkpoints/v1", summaryPath)
	}
	t.Logf("Condensation verified: checkpoint %s exists on entire/checkpoints/v1", checkpointID)

	// Verify transcript was condensed
	transcriptPath := SessionFilePath(checkpointID, "full.jsonl")
	if !env.FileExistsInBranch(paths.MetadataBranchName, transcriptPath) {
		t.Error("Transcript should be condensed to entire/checkpoints/v1")
	}

	// Step 6: Verify session stays ACTIVE after mid-turn commit
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist after commit")
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase after mid-turn commit should stay %q, got %q",
			session.PhaseActive, state.Phase)
	}

	// Step 7: Agent stops — session should transition to IDLE cleanly
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist after stop")
	}
	if state.Phase != session.PhaseIdle {
		t.Errorf("Phase after stop should be %q, got %q",
			session.PhaseIdle, state.Phase)
	}
	t.Logf("Session transitioned to IDLE cleanly (StepCount: %d)", state.StepCount)
}

// TestAgentCommitMidTurn_NoCondensationWithoutTranscript tests the safety guard:
// when TranscriptPath is empty (legacy/edge case), mid-turn commits should NOT
// trigger condensation even if the session is ACTIVE.
//
// This ensures we don't blindly condense sessions that have no transcript data.
func TestAgentCommitMidTurn_NoCondensationWithoutTranscript(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	sess := env.NewSession()

	// Start session WITHOUT transcript path (legacy behavior)
	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Verify session is ACTIVE but has no TranscriptPath
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state is nil")
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase should be %q, got %q", session.PhaseActive, state.Phase)
	}
	if state.TranscriptPath != "" {
		t.Fatalf("TranscriptPath should be empty for this test, got %q", state.TranscriptPath)
	}

	// Agent creates a file and commits (no transcript, no shadow branch)
	env.WriteFile("no_transcript_file.txt", "content without transcript")
	env.GitCommitWithShadowHooksAsAgent("Commit without transcript", "no_transcript_file.txt")

	commitHash := env.GetHeadHash()

	// The agent commit fast path adds a trailer regardless, but PostCommit
	// should not condense because there's no transcript data to condense.
	// Check if condensation happened by looking for checkpoint data.
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)

	if checkpointID != "" {
		// Trailer may exist (agent fast path), but condensation should not have
		// succeeded — no checkpoint data on entire/checkpoints/v1.
		summaryPath := CheckpointSummaryPath(checkpointID)
		if env.BranchExists(paths.MetadataBranchName) && env.FileExistsInBranch(paths.MetadataBranchName, summaryPath) {
			t.Error("Condensation should not produce checkpoint data when TranscriptPath is empty")
		} else {
			t.Log("Correctly: trailer exists but no condensed checkpoint data (no transcript)")
		}
	} else {
		t.Log("Correctly: no checkpoint trailer added without transcript")
	}
}
