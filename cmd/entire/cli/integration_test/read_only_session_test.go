//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestReadOnlySession_NotCondensed verifies that a session which never touched
// any files is NOT condensed into a checkpoint when the user commits.
//
// This reproduces the "summarize" bug where tools like steipete/summarize spawn
// many rapid-fire Codex exec sessions that only read/analyze content without
// modifying files. Each session creates a full lifecycle (SessionStart →
// UserPromptSubmit → Stop) but never calls SaveStep and never modifies files.
// When the user later commits, these read-only sessions should be silently
// skipped — not condensed into the checkpoint.
//
// State machine transitions tested:
//   - IDLE + GitCommit → should NOT condense when FilesTouched is empty
//   - Only the session that actually touched files should appear in the checkpoint
func TestReadOnlySession_NotCondensed(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// ========================================
	// Phase 1: Create a read-only session (no file changes)
	// ========================================
	t.Log("Phase 1: Simulate a read-only session (e.g., codex exec from summarize)")

	readOnlySess := env.NewSession()

	// Start the session — this creates session state
	if err := env.SimulateUserPromptSubmit(readOnlySess.ID); err != nil {
		t.Fatalf("read-only session user-prompt-submit failed: %v", err)
	}

	// Create a transcript with NO file changes — just a question and answer
	readOnlySess.TranscriptBuilder.AddUserMessage("Summarize the README for this project")
	readOnlySess.TranscriptBuilder.AddAssistantMessage("This project is a CLI tool for managing checkpoints.")
	if err := readOnlySess.TranscriptBuilder.WriteToFile(readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("failed to write read-only transcript: %v", err)
	}

	// Stop the session — NO files were touched, NO SaveStep was called
	if err := env.SimulateStop(readOnlySess.ID, readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("read-only session stop failed: %v", err)
	}

	// Verify: session is IDLE with empty FilesTouched
	roState, err := env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState for read-only session failed: %v", err)
	}
	if roState.Phase != session.PhaseIdle {
		t.Fatalf("Read-only session should be IDLE, got %s", roState.Phase)
	}
	if len(roState.FilesTouched) != 0 {
		t.Fatalf("Read-only session should have empty FilesTouched, got %v", roState.FilesTouched)
	}

	// ========================================
	// Phase 2: Create a normal session that DOES touch files
	// ========================================
	t.Log("Phase 2: Simulate a normal coding session that modifies files")

	codingSess := env.NewSession()

	if err := env.SimulateUserPromptSubmit(codingSess.ID); err != nil {
		t.Fatalf("coding session user-prompt-submit failed: %v", err)
	}

	// Create a file and a transcript that records the change
	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")
	codingSess.CreateTranscript("Create feature function", []FileChange{
		{Path: "feature.go", Content: "package main\n\nfunc Feature() {}\n"},
	})

	if err := env.SimulateStop(codingSess.ID, codingSess.TranscriptPath); err != nil {
		t.Fatalf("coding session stop failed: %v", err)
	}

	// Verify: coding session has files touched
	csState, err := env.GetSessionState(codingSess.ID)
	if err != nil {
		t.Fatalf("GetSessionState for coding session failed: %v", err)
	}
	if len(csState.FilesTouched) == 0 {
		t.Fatal("Coding session should have non-empty FilesTouched")
	}

	// ========================================
	// Phase 3: User commits — only the coding session should be condensed
	// ========================================
	t.Log("Phase 3: User commits; read-only session should NOT be condensed")

	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	// Get the checkpoint ID from the commit
	commitHash := env.GetHeadHash()
	cpID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if cpID == "" {
		t.Fatal("Commit should have an Entire-Checkpoint trailer")
	}

	// ========================================
	// Phase 4: Verify the read-only session was NOT included in the checkpoint
	// ========================================
	t.Log("Phase 4: Verify checkpoint contains only the coding session")

	// Read the checkpoint summary from entire/checkpoints/v1
	summaryPath := CheckpointSummaryPath(cpID)
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, summaryPath)
	if !found {
		t.Fatalf("CheckpointSummary not found at %s", summaryPath)
	}

	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
		t.Fatalf("Failed to parse CheckpointSummary: %v", err)
	}

	// The checkpoint should contain exactly 1 session (the coding session)
	if len(summary.Sessions) != 1 {
		t.Errorf("Checkpoint should contain exactly 1 session (the coding session), got %d sessions", len(summary.Sessions))
		for i, s := range summary.Sessions {
			t.Logf("  Session %d: %s", i, s.Metadata)
		}
	}

	// Verify the included session is the coding session, not the read-only one
	env.AssertCheckpointContainsSession(t, summary, codingSess.ID)
	env.AssertCheckpointExcludesSession(t, summary, readOnlySess.ID)

	// The read-only session should NOT have been condensed — verify that none of
	// the session fields updated by condensation changed.
	roStateAfter, err := env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState for read-only session after commit failed: %v", err)
	}
	if roStateAfter.LastCheckpointID != roState.LastCheckpointID {
		t.Errorf("Read-only session LastCheckpointID changed from %q to %q — it was incorrectly condensed",
			roState.LastCheckpointID, roStateAfter.LastCheckpointID)
	}
	if roStateAfter.FullyCondensed != roState.FullyCondensed {
		t.Errorf("Read-only session FullyCondensed changed from %t to %t — it was incorrectly condensed",
			roState.FullyCondensed, roStateAfter.FullyCondensed)
	}
	if roStateAfter.CheckpointTranscriptStart != roState.CheckpointTranscriptStart {
		t.Errorf("Read-only session CheckpointTranscriptStart changed from %d to %d — it was incorrectly condensed",
			roState.CheckpointTranscriptStart, roStateAfter.CheckpointTranscriptStart)
	}
}

// TestReadOnlySession_ActiveDuringCommit_NotCondensed verifies that a session
// which is still ACTIVE (between UserPromptSubmit and Stop) but has not touched
// any files is NOT condensed when a commit happens mid-turn.
//
// This is the more realistic scenario for the summarize bug: the read-only session
// fires UserPromptSubmit and the commit happens before Stop. The session is ACTIVE
// with recent interaction, which normally bypasses the overlap check. But since
// no files were touched, it should still not be condensed.
func TestReadOnlySession_ActiveDuringCommit_NotCondensed(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// ========================================
	// Phase 1: Create a coding session, do work, stop
	// ========================================
	t.Log("Phase 1: Create a coding session with file changes")

	codingSess := env.NewSession()

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(codingSess.ID, codingSess.TranscriptPath); err != nil {
		t.Fatalf("coding session user-prompt-submit failed: %v", err)
	}

	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")
	codingSess.CreateTranscript("Create feature function", []FileChange{
		{Path: "feature.go", Content: "package main\n\nfunc Feature() {}\n"},
	})

	if err := env.SimulateStop(codingSess.ID, codingSess.TranscriptPath); err != nil {
		t.Fatalf("coding session stop failed: %v", err)
	}

	// ========================================
	// Phase 2: Start a read-only session — leave it ACTIVE (no Stop)
	// ========================================
	t.Log("Phase 2: Start read-only session, leave it ACTIVE")

	readOnlySess := env.NewSession()

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(readOnlySess.ID, readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("read-only session user-prompt-submit failed: %v", err)
	}

	// Write a transcript with NO file changes
	readOnlySess.TranscriptBuilder.AddUserMessage("Explain what this codebase does")
	readOnlySess.TranscriptBuilder.AddAssistantMessage("This codebase implements a CLI tool.")
	if err := readOnlySess.TranscriptBuilder.WriteToFile(readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("failed to write read-only transcript: %v", err)
	}

	// Verify it's ACTIVE
	roState, err := env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if roState.Phase != session.PhaseActive {
		t.Fatalf("Read-only session should be ACTIVE, got %s", roState.Phase)
	}
	if len(roState.FilesTouched) != 0 {
		t.Fatalf("Read-only session should have empty FilesTouched, got %v", roState.FilesTouched)
	}

	// ========================================
	// Phase 3: User commits while read-only session is still ACTIVE
	// ========================================
	t.Log("Phase 3: User commits while read-only session is ACTIVE")

	env.GitCommitWithShadowHooks("Add feature", "feature.go")

	commitHash := env.GetHeadHash()
	cpID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if cpID == "" {
		t.Fatal("Commit should have an Entire-Checkpoint trailer")
	}

	// ========================================
	// Phase 4: Verify read-only ACTIVE session was NOT included
	// ========================================
	t.Log("Phase 4: Verify checkpoint contains only the coding session")

	summaryPath := CheckpointSummaryPath(cpID)
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, summaryPath)
	if !found {
		t.Fatalf("CheckpointSummary not found at %s", summaryPath)
	}

	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
		t.Fatalf("Failed to parse CheckpointSummary: %v", err)
	}

	// The checkpoint should contain exactly 1 session (the coding session).
	// The ACTIVE read-only session should NOT be condensed even though it has
	// recent interaction — it touched no files.
	if len(summary.Sessions) != 1 {
		t.Errorf("Checkpoint should contain exactly 1 session (the coding session), got %d sessions", len(summary.Sessions))
		for i, s := range summary.Sessions {
			t.Logf("  Session %d: %s", i, s.Metadata)
		}
	}

	// Verify the checkpoint records exactly the committed file from the coding session
	if len(summary.FilesTouched) != 1 {
		t.Fatalf("Checkpoint should contain exactly 1 touched file (feature.go), got %d: %v", len(summary.FilesTouched), summary.FilesTouched)
	}
	if summary.FilesTouched[0] != "feature.go" {
		t.Errorf("Unexpected file in checkpoint files_touched: got %q, want %q", summary.FilesTouched[0], "feature.go")
	}
}

// TestReadOnlySession_ActiveAcrossMultipleCommits verifies that a read-only ACTIVE
// session survives multiple commits without being condensed or causing errors.
//
// After the first commit, updateBaseCommitIfChanged advances the session's BaseCommit
// to the new HEAD. On the second commit, the session should again be skipped (still no
// files touched). This test ensures the BaseCommit advancement is correct and the
// session doesn't accumulate into subsequent checkpoints.
func TestReadOnlySession_ActiveAcrossMultipleCommits(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// ========================================
	// Phase 1: Start a read-only ACTIVE session
	// ========================================
	t.Log("Phase 1: Start read-only session, leave it ACTIVE")

	readOnlySess := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(readOnlySess.ID, readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("read-only session user-prompt-submit failed: %v", err)
	}
	readOnlySess.TranscriptBuilder.AddUserMessage("Explain the codebase")
	readOnlySess.TranscriptBuilder.AddAssistantMessage("This is a CLI tool.")
	if err := readOnlySess.TranscriptBuilder.WriteToFile(readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// Verify ACTIVE with no files
	roState, err := env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if roState.Phase != session.PhaseActive {
		t.Fatalf("Expected ACTIVE, got %s", roState.Phase)
	}
	initialBaseCommit := roState.BaseCommit

	// ========================================
	// Phase 2: First coding session + commit
	// ========================================
	t.Log("Phase 2: First coding session and commit")

	sess1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sess1.ID); err != nil {
		t.Fatalf("session 1 user-prompt-submit failed: %v", err)
	}
	env.WriteFile("file1.go", "package main\n\nfunc One() {}\n")
	sess1.CreateTranscript("Create file1", []FileChange{
		{Path: "file1.go", Content: "package main\n\nfunc One() {}\n"},
	})
	if err := env.SimulateStop(sess1.ID, sess1.TranscriptPath); err != nil {
		t.Fatalf("session 1 stop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add file1", "file1.go")
	firstCommitHash := env.GetHeadHash()
	cpID1 := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if cpID1 == "" {
		t.Fatal("First commit should have checkpoint trailer")
	}

	// Verify: read-only session NOT in first checkpoint
	summary1Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpID1))
	if !found {
		t.Fatal("First checkpoint summary not found")
	}
	var summary1 checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summary1Content), &summary1); err != nil {
		t.Fatalf("Failed to parse first checkpoint: %v", err)
	}
	if len(summary1.Sessions) != 1 {
		t.Errorf("First checkpoint should have 1 session, got %d", len(summary1.Sessions))
	}

	// Verify: read-only session's BaseCommit was advanced
	roState, err = env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if roState.BaseCommit == initialBaseCommit {
		t.Error("Read-only session BaseCommit should have advanced after first commit")
	}
	if roState.Phase != session.PhaseActive {
		t.Errorf("Read-only session should still be ACTIVE, got %s", roState.Phase)
	}

	// ========================================
	// Phase 3: Second coding session + commit
	// ========================================
	t.Log("Phase 3: Second coding session and commit")

	sess2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(sess2.ID); err != nil {
		t.Fatalf("session 2 user-prompt-submit failed: %v", err)
	}
	env.WriteFile("file2.go", "package main\n\nfunc Two() {}\n")
	sess2.CreateTranscript("Create file2", []FileChange{
		{Path: "file2.go", Content: "package main\n\nfunc Two() {}\n"},
	})
	if err := env.SimulateStop(sess2.ID, sess2.TranscriptPath); err != nil {
		t.Fatalf("session 2 stop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add file2", "file2.go")
	secondCommitHash := env.GetHeadHash()
	cpID2 := env.GetCheckpointIDFromCommitMessage(secondCommitHash)
	if cpID2 == "" {
		t.Fatal("Second commit should have checkpoint trailer")
	}

	// Verify: read-only session NOT in second checkpoint either
	summary2Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, CheckpointSummaryPath(cpID2))
	if !found {
		t.Fatal("Second checkpoint summary not found")
	}
	var summary2 checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summary2Content), &summary2); err != nil {
		t.Fatalf("Failed to parse second checkpoint: %v", err)
	}
	if len(summary2.Sessions) != 1 {
		t.Errorf("Second checkpoint should have 1 session, got %d", len(summary2.Sessions))
	}

	// Verify: read-only session still ACTIVE, BaseCommit advanced again
	roState, err = env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if roState.Phase != session.PhaseActive {
		t.Errorf("Read-only session should still be ACTIVE after 2 commits, got %s", roState.Phase)
	}
	if roState.BaseCommit != secondCommitHash {
		t.Errorf("Read-only session BaseCommit should be %s (second commit), got %s",
			secondCommitHash, roState.BaseCommit)
	}
}

// TestMultipleReadOnlySessions_NoneCondensed simulates the summarize scenario
// where many rapid-fire read-only sessions are created, then a user commits.
// None of the read-only sessions should appear in the checkpoint.
func TestMultipleReadOnlySessions_NoneCondensed(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// ========================================
	// Phase 1: Create multiple read-only sessions (simulating summarize batch)
	// ========================================
	t.Log("Phase 1: Create 5 read-only sessions (simulating summarize batch runs)")

	for i := range 5 {
		sess := env.NewSession()

		if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
			t.Fatalf("read-only session %d user-prompt-submit failed: %v", i, err)
		}

		// Write a transcript with no file changes
		sess.TranscriptBuilder.AddUserMessage("Summarize this repository")
		sess.TranscriptBuilder.AddAssistantMessage("This is a summary.")
		if err := sess.TranscriptBuilder.WriteToFile(sess.TranscriptPath); err != nil {
			t.Fatalf("failed to write transcript for session %d: %v", i, err)
		}

		if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
			t.Fatalf("read-only session %d stop failed: %v", i, err)
		}
	}

	// ========================================
	// Phase 2: Create one real coding session
	// ========================================
	t.Log("Phase 2: Create a real coding session that modifies files")

	codingSess := env.NewSession()
	if err := env.SimulateUserPromptSubmit(codingSess.ID); err != nil {
		t.Fatalf("coding session user-prompt-submit failed: %v", err)
	}

	env.WriteFile("main.go", "package main\n\nfunc main() {}\n")
	codingSess.CreateTranscript("Create main function", []FileChange{
		{Path: "main.go", Content: "package main\n\nfunc main() {}\n"},
	})

	if err := env.SimulateStop(codingSess.ID, codingSess.TranscriptPath); err != nil {
		t.Fatalf("coding session stop failed: %v", err)
	}

	// ========================================
	// Phase 3: User commits
	// ========================================
	t.Log("Phase 3: User commits")

	env.GitCommitWithShadowHooks("Add main function", "main.go")

	commitHash := env.GetHeadHash()
	cpID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if cpID == "" {
		t.Fatal("Commit should have an Entire-Checkpoint trailer")
	}

	// ========================================
	// Phase 4: Verify only the coding session was condensed
	// ========================================
	t.Log("Phase 4: Verify checkpoint contains exactly 1 session")

	summaryPath := CheckpointSummaryPath(cpID)
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, summaryPath)
	if !found {
		t.Fatalf("CheckpointSummary not found at %s", summaryPath)
	}

	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
		t.Fatalf("Failed to parse CheckpointSummary: %v", err)
	}

	// Should have exactly 1 session — the coding session.
	// The 5 read-only sessions should all have been skipped.
	if len(summary.Sessions) != 1 {
		t.Errorf("Checkpoint should contain exactly 1 session, got %d sessions (read-only sessions were incorrectly condensed)", len(summary.Sessions))
	}

	// Verify the checkpoint's files_touched only contains the coding session's files
	expectedFiles := map[string]bool{"main.go": true}
	for _, f := range summary.FilesTouched {
		if !expectedFiles[f] {
			t.Errorf("Unexpected file in checkpoint files_touched: %q (likely from read-only session fallback)", f)
		}
	}
}

// TestAllReadOnlySessions_NoCheckpointCreated verifies that when ALL sessions are
// read-only (no session has shadow branch content), no checkpoint trailer is added
// and no condensation occurs. PrepareCommitMsg's filterSessionsWithNewContent
// filters out sessions that never called SaveStep, so no trailer is written.
// This documents the full end-to-end behavior: read-only sessions produce no
// checkpoints regardless of whether other coding sessions exist.
func TestAllReadOnlySessions_NoCheckpointCreated(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)

	// ========================================
	// Phase 1: Start a read-only ACTIVE session
	// ========================================
	t.Log("Phase 1: Start read-only session, leave it ACTIVE")

	readOnlySess := env.NewSession()

	if err := env.SimulateUserPromptSubmitWithTranscriptPath(readOnlySess.ID, readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("read-only session user-prompt-submit failed: %v", err)
	}

	// Write a transcript with NO file changes
	readOnlySess.TranscriptBuilder.AddUserMessage("Explain the architecture of this project")
	readOnlySess.TranscriptBuilder.AddAssistantMessage("This project uses a layered architecture with commands, strategies, and checkpoints.")
	if err := readOnlySess.TranscriptBuilder.WriteToFile(readOnlySess.TranscriptPath); err != nil {
		t.Fatalf("failed to write read-only transcript: %v", err)
	}

	// Verify ACTIVE with no files
	roState, err := env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if roState.Phase != session.PhaseActive {
		t.Fatalf("Expected ACTIVE, got %s", roState.Phase)
	}
	if len(roState.FilesTouched) != 0 {
		t.Fatalf("Should have empty FilesTouched, got %v", roState.FilesTouched)
	}

	// ========================================
	// Phase 2: User manually creates and commits a file (no coding session)
	// ========================================
	t.Log("Phase 2: User manually commits a file — no other session claims it")

	env.WriteFile("manual.txt", "manually created file\n")
	env.GitCommitWithShadowHooks("Add manual file", "manual.txt")

	// ========================================
	// Phase 3: Verify no checkpoint trailer was added
	// ========================================
	t.Log("Phase 3: Verify no checkpoint was created (read-only session has no content to condense)")

	commitHash := env.GetHeadHash()
	cpID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if cpID != "" {
		t.Errorf("Commit should NOT have an Entire-Checkpoint trailer when only read-only sessions exist, got %q", cpID)
	}

	// Verify the read-only session state is unchanged
	roStateAfter, err := env.GetSessionState(readOnlySess.ID)
	if err != nil {
		t.Fatalf("GetSessionState after commit failed: %v", err)
	}
	if roStateAfter.Phase != session.PhaseActive {
		t.Errorf("Read-only session should still be ACTIVE, got %s", roStateAfter.Phase)
	}
	if roStateAfter.StepCount != roState.StepCount {
		t.Errorf("Read-only session StepCount should be unchanged, was %d now %d",
			roState.StepCount, roStateAfter.StepCount)
	}
}
