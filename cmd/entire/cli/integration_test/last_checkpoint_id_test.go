//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestShadowStrategy_OneCheckpointPerCommit tests the 1:1 checkpoint model:
// each commit gets its own unique checkpoint ID. When a session touches multiple
// files and the user splits them across commits (IDLE session), only the first
// commit gets a checkpoint trailer (via condensation). The second commit has no
// associated session data to condense.
//
// Flow:
// 1. Claude session edits files A and B, then stops (IDLE)
// 2. User commits file A → condensation → unique checkpoint ID #1
// 3. User commits file B → no session content to condense → no trailer
func TestShadowStrategy_OneCheckpointPerCommit(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("fileA.txt", "content from Claude for file A")
	env.WriteFile("fileB.txt", "content from Claude for file B")

	session.CreateTranscript("Create files A and B", []FileChange{
		{Path: "fileA.txt", Content: "content from Claude for file A"},
		{Path: "fileB.txt", Content: "content from Claude for file B"},
	})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	headBefore := env.GetHeadHash()

	// First commit: file A (triggers condensation)
	env.GitCommitWithShadowHooks("Add file A from Claude session", "fileA.txt")

	firstCommitHash := env.GetHeadHash()
	if firstCommitHash == headBefore {
		t.Fatal("First commit was not created")
	}
	firstCheckpointID := env.GetCheckpointIDFromCommitMessage(firstCommitHash)
	if firstCheckpointID == "" {
		t.Fatal("First commit should have Entire-Checkpoint trailer")
	}
	t.Logf("First commit checkpoint ID: %s", firstCheckpointID)

	// Verify checkpoint exists on entire/checkpoints/v1
	checkpointPath := paths.CheckpointPath(id.MustCheckpointID(firstCheckpointID))
	if !env.FileExistsInBranch(paths.MetadataBranchName, checkpointPath+"/"+paths.MetadataFileName) {
		t.Errorf("Checkpoint metadata should exist at %s on %s branch",
			checkpointPath, paths.MetadataBranchName)
	}

	// Second commit: file B (IDLE session, no carry-forward → no trailer)
	env.GitCommitWithShadowHooks("Add file B from Claude session", "fileB.txt")

	secondCommitHash := env.GetHeadHash()
	if secondCommitHash == firstCommitHash {
		t.Fatal("Second commit was not created")
	}
	secondCheckpointID := env.GetCheckpointIDFromCommitMessage(secondCommitHash)

	// In the 1:1 model, the second commit should NOT have a checkpoint trailer
	// because the session was IDLE (no carry-forward for idle sessions).
	if secondCheckpointID != "" {
		t.Logf("Note: second commit has checkpoint ID %s (carry-forward may have activated)", secondCheckpointID)
		// If carry-forward is implemented for idle sessions in the future,
		// this assertion can be changed. For now, verify they're different.
		if firstCheckpointID == secondCheckpointID {
			t.Error("If both commits have trailers, they must have DIFFERENT checkpoint IDs (1:1 model)")
		}
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

// TestShadowStrategy_NewSessionIgnoresOldCheckpointIDs tests that when multiple
// sessions exist in the worktree, each session's commits get their own unique
// checkpoint IDs. Old session checkpoint IDs are never reused by new sessions.
func TestShadowStrategy_NewSessionIgnoresOldCheckpointIDs(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	// === Create an OLD session on the initial commit ===
	oldSession := env.NewSession()
	if err := env.SimulateUserPromptSubmit(oldSession.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (old session) failed: %v", err)
	}

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

	// Make an intermediate commit (moves HEAD forward)
	env.WriteFile("intermediate.txt", "unrelated change")
	env.GitAdd("intermediate.txt")
	env.GitCommit("Intermediate commit (no session)")

	// === Create a NEW session on the new HEAD ===
	newSession := env.NewSession()
	if err := env.SimulateUserPromptSubmit(newSession.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (new session) failed: %v", err)
	}

	env.WriteFile("fileA.txt", "new session file A")
	newSession.CreateTranscript("Create new file A", []FileChange{
		{Path: "fileA.txt", Content: "new session file A"},
	})

	if err := env.SimulateStop(newSession.ID, newSession.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (new session) failed: %v", err)
	}

	// Commit from new session
	env.GitCommitWithShadowHooks("Add file A from new session", "fileA.txt")
	newCheckpointID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if newCheckpointID == "" {
		t.Fatal("New session commit should have checkpoint ID")
	}
	t.Logf("New session checkpoint ID: %s", newCheckpointID)

	// CRITICAL: New session checkpoint should NOT be the old session's checkpoint
	if newCheckpointID == oldCheckpointID {
		t.Errorf("New session commit reused old session checkpoint ID %s (should generate new ID)",
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
	// Shadow branch uses worktree-specific naming
	shadowBranchName := env.GetShadowBranchNameForCommit(state.BaseCommit)

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

	// Verify data exists on entire/checkpoints/v1
	checkpointID := env.GetLatestCheckpointID()
	checkpointPath := paths.CheckpointPath(id.MustCheckpointID(checkpointID))
	if !env.FileExistsInBranch(paths.MetadataBranchName, checkpointPath+"/"+paths.MetadataFileName) {
		t.Error("Checkpoint metadata should exist on entire/checkpoints/v1 branch")
	}
}

// TestShadowStrategy_BaseCommitUpdatedAfterCondensation tests that BaseCommit
// is updated to the new HEAD after condensation. This is essential for the 1:1
// checkpoint model where each commit gets its own unique checkpoint.
func TestShadowStrategy_BaseCommitUpdatedAfterCondensation(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	session := env.NewSession()

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("feature.go", "package main\nfunc Feature() {}\n")

	session.CreateTranscript("Create feature file", []FileChange{
		{Path: "feature.go", Content: "package main\nfunc Feature() {}\n"},
	})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Get BaseCommit before commit
	stateBefore, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state: %v", err)
	}
	baseCommitBefore := stateBefore.BaseCommit

	// Commit with hooks (triggers condensation)
	env.GitCommitWithShadowHooks("Add feature", "feature.go")
	commitHash := env.GetHeadHash()

	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	if checkpointID == "" {
		t.Fatal("Commit should have Entire-Checkpoint trailer")
	}
	t.Logf("Commit: %s, checkpoint: %s", commitHash[:7], checkpointID)

	// BaseCommit should advance from pre-commit value to new HEAD
	stateAfter, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("Failed to get session state after commit: %v", err)
	}

	if stateAfter.BaseCommit == baseCommitBefore {
		t.Error("BaseCommit should have changed after condensation")
	}

	if !strings.HasPrefix(commitHash, stateAfter.BaseCommit) {
		t.Errorf("BaseCommit should match HEAD: got %s, want prefix of %s",
			stateAfter.BaseCommit[:7], commitHash[:7])
	}
}
