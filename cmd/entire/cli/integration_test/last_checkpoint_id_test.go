//go:build integration

package integration

import (
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
)

// TestShadowStrategy_LastCheckpointID_ReusedAcrossCommits tests that when a user
// commits Claude's work across multiple commits without entering new prompts,
// all commits reference the same checkpoint ID.
//
// Flow:
// 1. Claude session edits files A and B
// 2. User commits file A (with hooks) → condensation, LastCheckpointID saved
// 3. User commits file B (with hooks) → no new content, reuses LastCheckpointID
// 4. Both commits should have the same Entire-Checkpoint trailer
func TestShadowStrategy_LastCheckpointID_ReusedAcrossCommits(t *testing.T) {
	t.Parallel()

	// Only test manual-commit strategy since this is shadow-specific behavior
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Create a session
	session := env.NewSession()

	// Simulate user prompt submit (initializes session)
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create two files as if Claude wrote them
	env.WriteFile("fileA.txt", "content from Claude for file A")
	env.WriteFile("fileB.txt", "content from Claude for file B")

	// Create transcript that shows Claude created both files
	session.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.txt", Content: "content from Claude for file A"},
		{Path: "fileB.txt", Content: "content from Claude for file B"},
	})

	// Simulate stop (creates checkpoint on shadow branch)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Get HEAD before first commit
	headBefore := env.GetHeadHash()

	// First commit: only file A (with shadow hooks - will trigger condensation)
	env.GitCommitWithShadowHooks("Add file A from Claude session", "fileA.txt")

	// Get the checkpoint ID from first commit
	firstCommitHash := env.GetHeadHash()
	if firstCommitHash == headBefore {
		t.Fatal("First commit was not created")
	}
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have Entire-Checkpoint trailer")
	}
	t.Logf("First commit checkpoint ID: %s", firstCheckpointID)

	// Verify LastCheckpointID was saved in session state
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist after first commit")
	}
	if state.LastCheckpointID.String() != firstCheckpointID {
		t.Errorf("Session state LastCheckpointID = %q, want %q", state.LastCheckpointID, firstCheckpointID)
	}

	// Second commit: file B (with hooks, but no new Claude activity)
	// Should reuse the same checkpoint ID
	env.GitCommitWithShadowHooks("Add file B from Claude session", "fileB.txt")

	// Get the checkpoint ID from second commit
	secondCommitHash := env.GetHeadHash()
	if secondCommitHash == firstCommitHash {
		t.Fatal("Second commit was not created")
	}
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)
	if secondCheckpointID == "" {
		t.Fatal("Second commit should have Entire-Checkpoint trailer")
	}
	t.Logf("Second commit checkpoint ID: %s", secondCheckpointID)

	// Both commits should have the SAME checkpoint ID
	if firstCheckpointID != secondCheckpointID {
		t.Errorf("Checkpoint IDs should match across commits:\n  First:  %s\n  Second: %s",
			firstCheckpointID, secondCheckpointID)
	}

	// Verify the checkpoint exists on entire/sessions branch
	checkpointPath := paths.CheckpointPath(id.MustCheckpointID(firstCheckpointID))
	if !env.FileExistsInBranch(paths.MetadataBranchName, checkpointPath+"/"+paths.MetadataFileName) {
		t.Errorf("Checkpoint metadata should exist at %s on %s branch",
			checkpointPath, paths.MetadataBranchName)
	}
}

// TestShadowStrategy_LastCheckpointID_ClearedOnNewPrompt tests that when a user
// enters a new prompt after committing, the LastCheckpointID is cleared and a
// fresh checkpoint ID is generated for subsequent commits.
func TestShadowStrategy_LastCheckpointID_ClearedOnNewPrompt(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// === First session work ===
	session1 := env.NewSession()

	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("first.txt", "first file")
	session1.CreateTranscript("Create first file", []FileChange{
		{Path: "first.txt", Content: "first file"},
	})

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit first file
	env.GitCommitWithShadowHooks("First commit", "first.txt")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	t.Logf("First checkpoint ID: %s", firstCheckpointID)

	// Verify LastCheckpointID is set
	state, err := env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	if state.LastCheckpointID.String() != firstCheckpointID {
		t.Errorf("LastCheckpointID should be set to %s, got %s", firstCheckpointID, state.LastCheckpointID)
	}

	// === User continues session (enters new prompt) ===
	// This should update BaseCommit and clear LastCheckpointID
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (second prompt) failed: %v", err)
	}

	// Verify LastCheckpointID was cleared
	state, err = env.GetSessionState(session1.ID)
	if err != nil {
		t.Fatalf("Failed to get session state after second prompt: %v", err)
	}
	if state.LastCheckpointID != "" {
		t.Errorf("LastCheckpointID should be cleared after new prompt, got %q", state.LastCheckpointID)
	}

	// Verify BaseCommit was updated to the new HEAD
	if state.BaseCommit != firstCommitHash[:7] && !strings.HasPrefix(firstCommitHash, state.BaseCommit) {
		t.Errorf("BaseCommit should be updated to new HEAD, got %s (HEAD: %s)", state.BaseCommit, firstCommitHash)
	}

	// === Second session work ===
	env.WriteFile("second.txt", "second file")
	session1.TranscriptBuilder.AddUserMessage("Create second file")
	session1.TranscriptBuilder.AddAssistantMessage("Creating second file")
	toolID := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "second.txt", "second file")
	session1.TranscriptBuilder.AddToolResult(toolID)
	if err := session1.TranscriptBuilder.WriteToFile(session1.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (second) failed: %v", err)
	}

	// Commit second file
	env.GitCommitWithShadowHooks("Second commit", "second.txt")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)
	t.Logf("Second checkpoint ID: %s", secondCheckpointID)

	// The checkpoint IDs should be DIFFERENT because we entered a new prompt
	if firstCheckpointID == secondCheckpointID {
		t.Errorf("Checkpoint IDs should be different after new prompt:\n  First:  %s\n  Second: %s",
			firstCheckpointID, secondCheckpointID)
	}
}

// TestShadowStrategy_LastCheckpointID_NotSetWithoutCondensation tests that
// LastCheckpointID is not set when committing without session activity.
func TestShadowStrategy_LastCheckpointID_NotSetWithoutCondensation(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Create a file directly (not through a Claude session)
	env.WriteFile("manual.txt", "manual content")

	// Commit with shadow hooks - should not add trailer since no session exists
	env.GitCommitWithShadowHooks("Manual commit without session", "manual.txt")

	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)

	// No session activity, so no checkpoint ID should be added
	if checkpointID != "" {
		t.Errorf("Commit without session should not have checkpoint ID, got %q", checkpointID)
	}
}

// TestShadowStrategy_LastCheckpointID_IgnoresOldSessions tests that when multiple
// old sessions exist in the worktree, only the current session (matching BaseCommit)
// is used for checkpoint ID reuse.
//
// This reproduces the bug where old session states from previous days would cause
// different checkpoint IDs to be used when making multiple commits from a new session.
func TestShadowStrategy_LastCheckpointID_IgnoresOldSessions(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// === Create an OLD session on the initial commit ===
	oldSession := env.NewSession()
	if err := env.SimulateUserPromptSubmit(oldSession.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (old session) failed: %v", err)
	}

	// Old session modifies a file
	env.WriteFile("old.txt", "old session content")
	oldSession.CreateTranscript("Create old file", []FileChange{
		{Path: "old.txt", Content: "old session content"},
	})

	if err := env.SimulateStop(oldSession.ID, oldSession.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (old session) failed: %v", err)
	}

	// Commit from old session
	env.GitCommitWithShadowHooks("Old session commit", "old.txt")
	oldCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if oldCheckpointID == "" {
		t.Fatal("Old session commit should have checkpoint ID")
	}
	t.Logf("Old session checkpoint ID: %s", oldCheckpointID)

	// Make an intermediate commit (moves HEAD forward, creates new base for new session)
	env.WriteFile("intermediate.txt", "unrelated change")
	env.GitAdd("intermediate.txt")
	env.GitCommit("Intermediate commit (no session)")

	// === Create a NEW session on the new HEAD ===
	newSession := env.NewSession()
	if err := env.SimulateUserPromptSubmit(newSession.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (new session) failed: %v", err)
	}

	// New session modifies two files
	env.WriteFile("fileA.txt", "new session file A")
	env.WriteFile("fileB.txt", "new session file B")
	newSession.CreateTranscript("Create new files A and B", []FileChange{
		{Path: "fileA.txt", Content: "new session file A"},
		{Path: "fileB.txt", Content: "new session file B"},
	})

	if err := env.SimulateStop(newSession.ID, newSession.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (new session) failed: %v", err)
	}

	// At this point, we have TWO session state files:
	// - Old session: BaseCommit = old HEAD, LastCheckpointID = oldCheckpointID
	// - New session: BaseCommit = current HEAD, FilesTouched = ["fileA.txt", "fileB.txt"]

	// First commit from new session
	env.GitCommitWithShadowHooks("Add file A from new session", "fileA.txt")
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if firstCheckpointID == "" {
		t.Fatal("First commit should have checkpoint ID")
	}
	t.Logf("First new session checkpoint ID: %s", firstCheckpointID)

	// CRITICAL: First checkpoint should NOT be the old session's checkpoint
	if firstCheckpointID == oldCheckpointID {
		t.Errorf("First new session commit reused old session checkpoint ID %s (should generate new ID)",
			oldCheckpointID)
	}

	// Second commit from new session
	env.GitCommitWithShadowHooks("Add file B from new session", "fileB.txt")
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if secondCheckpointID == "" {
		t.Fatal("Second commit should have checkpoint ID")
	}
	t.Logf("Second new session checkpoint ID: %s", secondCheckpointID)

	// CRITICAL: Both commits from new session should have SAME checkpoint ID
	if firstCheckpointID != secondCheckpointID {
		t.Errorf("New session commits should have same checkpoint ID:\n  First:  %s\n  Second: %s",
			firstCheckpointID, secondCheckpointID)
	}

	// CRITICAL: Neither should be the old session's checkpoint ID
	if secondCheckpointID == oldCheckpointID {
		t.Errorf("Second new session commit reused old session checkpoint ID %s",
			oldCheckpointID)
	}
}

// TestShadowStrategy_ShadowBranchCleanedUpAfterCondensation verifies that the
// shadow branch is deleted after successful condensation.
func TestShadowStrategy_ShadowBranchCleanedUpAfterCondensation(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Get the base commit to determine shadow branch name
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	// Shadow branch uses 7-char prefix of base commit
	baseCommitPrefix := state.BaseCommit
	if len(baseCommitPrefix) > 7 {
		baseCommitPrefix = baseCommitPrefix[:7]
	}
	shadowBranchName := "entire/" + baseCommitPrefix

	env.WriteFile("test.txt", "test content")
	session.CreateTranscript("Create test file", []FileChange{
		{Path: "test.txt", Content: "test content"},
	})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify shadow branch exists before commit
	if !env.BranchExists(shadowBranchName) {
		t.Fatalf("Shadow branch %s should exist before commit", shadowBranchName)
	}

	// Commit with hooks (triggers condensation and cleanup)
	env.GitCommitWithShadowHooks("Test commit", "test.txt")

	// Verify shadow branch was cleaned up
	if env.BranchExists(shadowBranchName) {
		t.Errorf("Shadow branch %s should be deleted after condensation", shadowBranchName)
	}

	// Verify data exists on entire/sessions
	checkpointID := env.GetLatestCheckpointID()
	checkpointPath := paths.CheckpointPath(id.MustCheckpointID(checkpointID))
	if !env.FileExistsInBranch(paths.MetadataBranchName, checkpointPath+"/"+paths.MetadataFileName) {
		t.Error("Checkpoint metadata should exist on entire/sessions branch")
	}
}

// TestShadowStrategy_BaseCommitUpdatedOnReuse tests that BaseCommit is updated
// even when a commit reuses a previous checkpoint ID (no new content to condense).
// This prevents the stale BaseCommit bug where subsequent commits would fall back
// to old sessions because no sessions matched the current HEAD.
func TestShadowStrategy_BaseCommitUpdatedOnReuse(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// Create a session
	session := env.NewSession()

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Claude creates two files
	env.WriteFile("fileA.txt", "content A")
	env.WriteFile("fileB.txt", "content B")

	session.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.txt", Content: "content A"},
		{Path: "fileB.txt", Content: "content B"},
	})

	// Stop (creates checkpoint on shadow branch)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// First commit: file A (with hooks - triggers condensation)
	env.GitCommitWithShadowHooks("Add file A", "fileA.txt")
	firstCommitHash := env.GetHeadHash()
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	t.Logf("First commit (condensed): %s, checkpoint: %s", firstCommitHash[:7], firstCheckpointID)

	// Get session state after first commit
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	baseCommitAfterFirst := state.BaseCommit
	t.Logf("BaseCommit after first commit: %s", baseCommitAfterFirst[:7])

	// Verify BaseCommit matches first commit
	if !strings.HasPrefix(firstCommitHash, baseCommitAfterFirst) {
		t.Errorf("BaseCommit after first commit should match HEAD: got %s, want prefix of %s",
			baseCommitAfterFirst[:7], firstCommitHash[:7])
	}

	// Second commit: file B (reuse - no new content to condense)
	env.GitCommitWithShadowHooks("Add file B", "fileB.txt")
	secondCommitHash := env.GetHeadHash()
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)
	t.Logf("Second commit (reuse): %s, checkpoint: %s", secondCommitHash[:7], secondCheckpointID)

	// Verify checkpoint IDs match (reuse is correct)
	if firstCheckpointID != secondCheckpointID {
		t.Errorf("Second commit should reuse first checkpoint ID: got %s, want %s",
			secondCheckpointID, firstCheckpointID)
	}

	// CRITICAL: Get session state after second commit
	// BaseCommit should be updated to second commit hash, not stay at first commit
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state after second commit: %v", err)
	}
	baseCommitAfterSecond := state.BaseCommit
	t.Logf("BaseCommit after second commit: %s", baseCommitAfterSecond[:7])

	// REGRESSION TEST: BaseCommit must be updated even without condensation
	// Before the fix, BaseCommit stayed at firstCommitHash after reuse commits
	if !strings.HasPrefix(secondCommitHash, baseCommitAfterSecond) {
		t.Errorf("BaseCommit after reuse commit should match HEAD: got %s, want prefix of %s\n"+
			"This is a regression: BaseCommit was not updated after commit without condensation",
			baseCommitAfterSecond[:7], secondCommitHash[:7])
	}
}
