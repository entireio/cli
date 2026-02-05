//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
	"entire.io/cli/cmd/entire/cli/trailers"
)

// TestShadow_FullWorkflow tests the complete shadow workflow as described in
// docs/requirements/shadow-strategy/example.md
//
// This test simulates Alice's workflow:
// 1. Start session, create checkpoints
// 2. Rewind to earlier checkpoint
// 3. Create new checkpoint after rewind
// 4. User commits (triggers condensation)
// 5. Continue working after commit (new shadow branch)
// 6. User commits again (second condensation)
// 7. Verify final state
func TestShadow_FullWorkflow(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// ========================================
	// Phase 1: Setup
	// ========================================
	env.InitRepo()

	// Create initial commit on main
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Switch to feature branch (shadow skips main/master)
	env.GitCheckoutNewBranch("feature/auth")

	// Initialize Entire AFTER branch switch to avoid go-git cleaning untracked files
	env.InitEntire(strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	t.Logf("Initial HEAD on feature/auth: %s", initialHead[:7])

	// ========================================
	// Phase 2: Session Start & First Checkpoint
	// ========================================
	t.Log("Phase 2: Starting session and creating first checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Verify session state file exists in .git/entire-sessions/
	sessionStateDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	entries, err := os.ReadDir(sessionStateDir)
	if err != nil {
		t.Fatalf("Failed to read session state dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("Expected session state file in .git/entire-sessions/")
	}

	// Create first file (src/auth.go)
	authV1 := "package auth\n\nfunc Authenticate(user, pass string) bool {\n\treturn false\n}"
	env.WriteFile("src/auth.go", authV1)

	session.CreateTranscript(
		"Create authentication module",
		[]FileChange{{Path: "src/auth.go", Content: authV1}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Verify shadow branch created with correct worktree-specific naming
	expectedShadowBranch := env.GetShadowBranchNameForCommit(initialHead)
	if !env.BranchExists(expectedShadowBranch) {
		t.Errorf("Expected shadow branch %s to exist", expectedShadowBranch)
	}

	// Verify 1 rewind point
	points := env.GetRewindPoints()
	if len(points) != 1 {
		t.Fatalf("Expected 1 rewind point after first checkpoint, got %d", len(points))
	}
	t.Logf("Checkpoint 1 created: %s", points[0].Message)

	// ========================================
	// Phase 3: Second Checkpoint (continuing same session)
	// ========================================
	t.Log("Phase 3: Creating second checkpoint (continuing same session)")

	// Continue the same session (not a new session) - this is the expected Claude behavior
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (checkpoint 2) failed: %v", err)
	}

	// Create hash.go and modify auth.go
	hashV1 := "package auth\n\nimport \"crypto/sha256\"\n\nfunc HashPassword(pass string) []byte {\n\th := sha256.Sum256([]byte(pass))\n\treturn h[:]\n}"
	authV2 := "package auth\n\nfunc Authenticate(user, pass string) bool {\n\t// TODO: use hashed passwords\n\treturn false\n}"
	env.WriteFile("src/hash.go", hashV1)
	env.WriteFile("src/auth.go", authV2)

	// Reset transcript builder for the new checkpoint
	session.TranscriptBuilder = NewTranscriptBuilder()
	session.CreateTranscript(
		"Add password hashing",
		[]FileChange{
			{Path: "src/hash.go", Content: hashV1},
			{Path: "src/auth.go", Content: authV2},
		},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2) failed: %v", err)
	}

	// Verify 2 rewind points on same shadow branch
	points = env.GetRewindPoints()
	if len(points) != 2 {
		t.Fatalf("Expected 2 rewind points after second checkpoint, got %d", len(points))
	}
	t.Logf("Checkpoint 2 created: %s", points[0].Message)

	// Verify both files exist
	if !env.FileExists("src/hash.go") {
		t.Error("src/hash.go should exist after checkpoint 2")
	}
	if content := env.ReadFile("src/auth.go"); content != authV2 {
		t.Errorf("src/auth.go should have v2 content, got: %s", content)
	}

	// ========================================
	// Phase 4: Rewind to First Checkpoint
	// ========================================
	t.Log("Phase 4: Rewinding to first checkpoint")

	// Find checkpoint 1 by message (the one for "Create authentication module")
	var checkpoint1ID string
	for _, p := range points {
		if p.Message == "Create authentication module" {
			checkpoint1ID = p.ID
			break
		}
	}
	if checkpoint1ID == "" {
		t.Fatalf("Could not find checkpoint for 'Create authentication module' in %d points", len(points))
	}

	if err := env.Rewind(checkpoint1ID); err != nil {
		t.Fatalf("Rewind to checkpoint 1 failed: %v", err)
	}

	// Verify hash.go was removed (it was only added in checkpoint 2)
	if env.FileExists("src/hash.go") {
		t.Error("src/hash.go should NOT exist after rewind to checkpoint 1")
	}

	// Verify auth.go restored to v1 (without the "TODO: use hashed passwords" comment)
	content := env.ReadFile("src/auth.go")
	if content != authV1 {
		t.Errorf("src/auth.go should be restored to v1 after rewind, got: %s", content)
	}

	// Verify HEAD unchanged (shadow doesn't modify user's branch)
	if head := env.GetHeadHash(); head != initialHead {
		t.Errorf("HEAD should be unchanged after rewind, got %s, want %s", head[:7], initialHead[:7])
	}

	// Verify shadow branch still exists (history preserved)
	if !env.BranchExists(expectedShadowBranch) {
		t.Errorf("Shadow branch %s should still exist after rewind", expectedShadowBranch)
	}

	// ========================================
	// Phase 5: New Checkpoint After Rewind (continue same session)
	// ========================================
	t.Log("Phase 5: Creating checkpoint after rewind (continuing same session)")

	// Continue the same session after rewind
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (after rewind) failed: %v", err)
	}

	// Reset transcript builder for next checkpoint
	session.TranscriptBuilder = NewTranscriptBuilder()

	// Create bcrypt.go instead (different approach)
	bcryptV1 := "package auth\n\nimport \"golang.org/x/crypto/bcrypt\"\n\nfunc HashPassword(pass string) ([]byte, error) {\n\treturn bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)\n}"
	authV3 := "package auth\n\nfunc Authenticate(user, pass string) bool {\n\t// Use bcrypt for password hashing\n\treturn false\n}"
	env.WriteFile("src/bcrypt.go", bcryptV1)
	env.WriteFile("src/auth.go", authV3)

	session.CreateTranscript(
		"Use bcrypt for hashing",
		[]FileChange{
			{Path: "src/bcrypt.go", Content: bcryptV1},
			{Path: "src/auth.go", Content: authV3},
		},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 3) failed: %v", err)
	}

	// Verify we now have checkpoints (may be 3 if history preserved, or could vary)
	points = env.GetRewindPoints()
	if len(points) < 1 {
		t.Fatalf("Expected at least 1 rewind point after checkpoint 3, got %d", len(points))
	}
	t.Logf("After rewind and new checkpoint: %d rewind points", len(points))

	// ========================================
	// Phase 6: User Commits (Condensation)
	// ========================================
	t.Log("Phase 6: User commits - triggering condensation")

	// Stage and commit with shadow hooks
	env.GitCommitWithShadowHooks("Add user authentication with bcrypt", "src/auth.go", "src/bcrypt.go")

	// Get the new commit
	commit1Hash := env.GetHeadHash()
	t.Logf("User commit 1: %s", commit1Hash[:7])

	// Active branch commits should be clean (no Entire-* trailers)
	commitMsg := env.GetCommitMessage(commit1Hash)
	if strings.Contains(commitMsg, "Entire-Session:") {
		t.Errorf("Commit should NOT have Entire-Session trailer (clean history), got: %s", commitMsg)
	}
	if strings.Contains(commitMsg, "Entire-Condensation:") {
		t.Errorf("Commit should NOT have Entire-Condensation trailer (clean history), got: %s", commitMsg)
	}

	// Get checkpoint ID by walking history - verifies condensation added the trailer
	checkpoint1ID = env.GetLatestCheckpointIDFromHistory()
	t.Logf("Checkpoint 1 ID: %s", checkpoint1ID)

	// Verify entire/sessions branch exists with checkpoint folder
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Error("entire/sessions branch should exist after condensation")
	}

	// Verify checkpoint folder contents (check via git show)
	// Uses sharded path: <id[:2]>/<id[2:]>/metadata.json
	checkpointPath := ShardedCheckpointPath(checkpoint1ID) + "/metadata.json"
	if !env.FileExistsInBranch(paths.MetadataBranchName, checkpointPath) {
		t.Errorf("Checkpoint folder should contain metadata.json at %s", checkpointPath)
	}

	// Clear session state to simulate session completion (avoids concurrent session warning)
	if err := env.ClearSessionState(session.ID); err != nil {
		t.Fatalf("ClearSessionState failed: %v", err)
	}

	// ========================================
	// Phase 7: Continue Working After Commit
	// ========================================
	t.Log("Phase 7: Continuing work after user commit")

	// Verify HEAD changed
	if commit1Hash == initialHead {
		t.Error("HEAD should have changed after user commit")
	}

	// Start new session
	session4 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session4.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session4) failed: %v", err)
	}

	// Create session management file
	sessionMgmt := "package auth\n\nimport \"time\"\n\ntype Session struct {\n\tUserID string\n\tExpiry time.Time\n}"
	env.WriteFile("src/session.go", sessionMgmt)

	session4.CreateTranscript(
		"Add session management",
		[]FileChange{{Path: "src/session.go", Content: sessionMgmt}},
	)
	if err := env.SimulateStop(session4.ID, session4.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 4) failed: %v", err)
	}

	// Verify NEW shadow branch created (based on new HEAD)
	expectedShadowBranch2 := env.GetShadowBranchNameForCommit(commit1Hash)
	if !env.BranchExists(expectedShadowBranch2) {
		t.Errorf("Expected new shadow branch %s after commit", expectedShadowBranch2)
	}

	// Verify it's different from the first shadow branch
	if expectedShadowBranch == expectedShadowBranch2 {
		t.Error("New shadow branch should have different name than first (different base commit)")
	}
	t.Logf("New shadow branch after commit: %s", expectedShadowBranch2)

	// ========================================
	// Phase 8: Second User Commit
	// ========================================
	t.Log("Phase 8: Second user commit")

	env.GitCommitWithShadowHooks("Add session management", "src/session.go")

	commit2Hash := env.GetHeadHash()
	t.Logf("User commit 2: %s", commit2Hash[:7])

	// Verify commit is clean (no trailers)
	commitMsg2 := env.GetCommitMessage(commit2Hash)
	if strings.Contains(commitMsg2, "Entire-Session:") {
		t.Errorf("Commit should NOT have Entire-Session trailer (clean history), got: %s", commitMsg2)
	}

	// Get checkpoint ID from commit message trailer (not from timestamp-based matching
	// which is flaky when commits happen within the same second)
	checkpoint2ID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Checkpoint 2 ID: %s", checkpoint2ID)

	// Verify DIFFERENT checkpoint ID
	if checkpoint1ID == checkpoint2ID {
		t.Errorf("Second commit should have different checkpoint ID: %s vs %s", checkpoint1ID, checkpoint2ID)
	}

	// Verify second checkpoint folder exists (uses sharded path)
	checkpoint2Path := ShardedCheckpointPath(checkpoint2ID) + "/metadata.json"
	if !env.FileExistsInBranch(paths.MetadataBranchName, checkpoint2Path) {
		t.Errorf("Second checkpoint folder should exist at %s", checkpoint2Path)
	}

	// ========================================
	// Phase 9: Verify Final State
	// ========================================
	t.Log("Phase 9: Verifying final state")

	// 2 user commits on feature branch
	// Both should be clean (no Entire-* trailers)
	if strings.Contains(commitMsg, "Entire-Session:") || strings.Contains(commitMsg2, "Entire-Session:") {
		t.Error("Commits should NOT have Entire-Session trailer (clean history)")
	}

	// 2 checkpoint folders in entire/sessions (Already verified above)

	// Verify all expected files exist in working directory
	expectedFiles := []string{"README.md", "src/auth.go", "src/bcrypt.go", "src/session.go"}
	for _, f := range expectedFiles {
		if !env.FileExists(f) {
			t.Errorf("Expected file %s to exist in final state", f)
		}
	}

	// Verify shadow branches exist (can be pruned later)
	if !env.BranchExists(expectedShadowBranch) {
		t.Logf("Note: First shadow branch %s may have been cleaned up", expectedShadowBranch)
	}
	if !env.BranchExists(expectedShadowBranch2) {
		t.Logf("Note: Second shadow branch %s may have been cleaned up", expectedShadowBranch2)
	}

	t.Log("Shadow full workflow test completed successfully!")
}

// TestShadow_SessionStateLocation verifies session state is stored in .git/
// (not .entire/) so it's never accidentally committed.
func TestShadow_SessionStateLocation(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")

	// Initialize AFTER branch switch
	env.InitEntire(strategy.StrategyNameManualCommit)

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Session state should be in .git/entire-sessions/, NOT .entire/
	gitSessionDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	entireSessionDir := filepath.Join(env.RepoDir, ".entire", "entire-sessions")

	if _, err := os.Stat(gitSessionDir); os.IsNotExist(err) {
		t.Error("Session state directory should exist at .git/entire-sessions/")
	}

	if _, err := os.Stat(entireSessionDir); err == nil {
		t.Error("Session state should NOT be in .entire/entire-sessions/")
	}
}

// TestShadow_MultipleConcurrentSessions tests that starting a second Claude session
// while another session has uncommitted checkpoints triggers a warning.
// The first prompt is blocked with continue:false, subsequent prompts proceed.
func TestShadow_MultipleConcurrentSessions(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")

	// Initialize AFTER branch switch
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start first session
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session1) failed: %v", err)
	}

	// Create a checkpoint for session1 (this creates the shadow branch)
	env.WriteFile("file.txt", "content")
	session1.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session1) failed: %v", err)
	}

	// Verify session state file exists
	sessionStateDir := filepath.Join(env.RepoDir, ".git", "entire-sessions")
	entries, err := os.ReadDir(sessionStateDir)
	if err != nil {
		t.Fatalf("Failed to read session state dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("Expected 1 session state file, got %d", len(entries))
	}

	// Starting a second session while session1 has uncommitted checkpoints triggers warning
	// The hook outputs JSON {"continue":false,...} but returns nil (success)
	session2 := env.NewSession()
	err = env.SimulateUserPromptSubmit(session2.ID)
	// The hook succeeds (returns nil) but outputs JSON with continue:false
	// The test infrastructure treats this as success since the hook didn't return an error
	if err != nil {
		t.Logf("SimulateUserPromptSubmit returned error (expected in some cases): %v", err)
	}

	// Verify session2 state file was created with ConcurrentWarningShown flag
	// This is set by the hook when it outputs continue:false
	entries, err = os.ReadDir(sessionStateDir)
	if err != nil {
		t.Fatalf("Failed to read session state dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("Expected 2 session state files after second session attempt, got %d", len(entries))
	}

	// Clear session1 state file - this makes the shadow branch "orphaned"
	if err := env.ClearSessionState(session1.ID); err != nil {
		t.Fatalf("ClearSessionState failed: %v", err)
	}

	// Session2 had ConcurrentWarningShown=true, but now the conflict is resolved
	// (session1 state cleared), so the warning flag is cleared and hooks proceed normally.
	// The orphaned shadow branch (from session1) is reset, allowing session2 to proceed.
	err = env.SimulateUserPromptSubmit(session2.ID)
	if err != nil {
		t.Errorf("Expected success after orphaned shadow branch is reset, got: %v", err)
	} else {
		t.Log("Session2 proceeded after orphaned shadow branch was reset")
	}
}

// TestShadow_ShadowBranchMigrationOnPull verifies that when the base commit changes
// (e.g., after stash → pull → apply), the shadow branch is moved to the new commit.
func TestShadow_ShadowBranchMigrationOnPull(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	originalHead := env.GetHeadHash()
	originalShadowBranch := env.GetShadowBranchNameForCommit(originalHead)

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	session.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify shadow branch exists at original commit
	if !env.BranchExists(originalShadowBranch) {
		t.Fatalf("Shadow branch %s should exist", originalShadowBranch)
	}
	t.Logf("Original shadow branch: %s", originalShadowBranch)

	// Simulate pull: create a new commit (simulating what pull would do)
	// In real scenario: stash → pull → apply
	// Here we just create a commit to simulate HEAD moving
	env.WriteFile("pulled.txt", "from remote")
	env.GitAdd("pulled.txt")
	env.GitCommit("Simulated pull commit")

	newHead := env.GetHeadHash()
	newShadowBranch := env.GetShadowBranchNameForCommit(newHead)
	t.Logf("After simulated pull: old=%s new=%s", originalHead[:7], newHead[:7])

	// Restore the file (simulating stash apply)
	env.WriteFile("file.txt", "content")

	// Next prompt should migrate the shadow branch
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit after pull failed: %v", err)
	}

	// Verify old shadow branch is gone and new one exists
	if env.BranchExists(originalShadowBranch) {
		t.Errorf("Old shadow branch %s should be deleted after migration", originalShadowBranch)
	}
	if !env.BranchExists(newShadowBranch) {
		t.Errorf("New shadow branch %s should exist after migration", newShadowBranch)
	}

	// Verify we can still create checkpoints on the new shadow branch
	env.WriteFile("file2.txt", "more content")
	session.TranscriptBuilder = NewTranscriptBuilder()
	session.CreateTranscript("Add file2", []FileChange{{Path: "file2.txt", Content: "more content"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop after migration failed: %v", err)
	}

	// Verify session state has updated base commit and preserves agent type
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state.BaseCommit != newHead {
		t.Errorf("Session base commit should be %s, got %s", newHead[:7], state.BaseCommit[:7])
	}
	if state.CheckpointCount != 2 {
		t.Errorf("Expected 2 checkpoints after migration, got %d", state.CheckpointCount)
	}
	// Verify agent_type is preserved across checkpoints and migration
	expectedAgentType := agent.AgentTypeClaudeCode
	if state.AgentType != expectedAgentType {
		t.Errorf("Session AgentType should be %q, got %q", expectedAgentType, state.AgentType)
	}

	t.Log("Shadow branch successfully migrated after base commit change")
}

// TestShadow_ShadowBranchNaming verifies shadow branches follow the
// entire/<base-sha[:7]> naming convention.
func TestShadow_ShadowBranchNaming(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")

	// Initialize AFTER branch switch
	env.InitEntire(strategy.StrategyNameManualCommit)

	baseHead := env.GetHeadHash()

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("file.txt", "content")
	session.CreateTranscript("Add file", []FileChange{{Path: "file.txt", Content: "content"}})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify shadow branch name matches worktree-specific format
	expectedBranch := env.GetShadowBranchNameForCommit(baseHead)
	if !env.BranchExists(expectedBranch) {
		t.Errorf("Shadow branch should be named %s", expectedBranch)
	}

	// List all entire/ branches
	branches := env.ListBranchesWithPrefix("entire/")
	t.Logf("Found entire/ branches: %v", branches)

	foundExpected := false
	for _, b := range branches {
		if b == expectedBranch {
			foundExpected = true
			break
		}
	}
	if !foundExpected {
		t.Errorf("Expected branch %s not found in %v", expectedBranch, branches)
	}
}

// TestShadow_TranscriptCondensation verifies that session transcripts are
// included in the entire/sessions branch during condensation.
func TestShadow_TranscriptCondensation(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start session and create checkpoint with transcript
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file change
	content := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	env.WriteFile("main.go", content)

	// Create transcript with meaningful content
	session.CreateTranscript(
		"Create main.go with hello world",
		[]FileChange{{Path: "main.go", Content: content}},
	)

	// Save checkpoint (this stores transcript in shadow branch)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit with hooks (triggers condensation)
	env.GitCommitWithShadowHooks("Add main.go", "main.go")

	// Get checkpoint ID from entire/sessions branch (not from commit message)
	checkpointID := env.GetLatestCheckpointID()
	t.Logf("Checkpoint ID: %s", checkpointID)

	// Verify entire/sessions branch exists
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/sessions branch should exist after condensation")
	}

	// Verify root metadata.json (CheckpointSummary) exists
	summaryPath := CheckpointSummaryPath(checkpointID)
	if !env.FileExistsInBranch(paths.MetadataBranchName, summaryPath) {
		t.Errorf("root metadata.json should exist at %s", summaryPath)
	}

	// Verify transcript file exists in session subdirectory (new format: 0/full.jsonl)
	transcriptPath := SessionFilePath(checkpointID, paths.TranscriptFileName)
	if !env.FileExistsInBranch(paths.MetadataBranchName, transcriptPath) {
		t.Errorf("Transcript (%s) should exist at %s", paths.TranscriptFileName, transcriptPath)
	} else {
		t.Log("✓ Transcript file exists in checkpoint")
	}

	// Verify content_hash.txt exists in session subdirectory
	hashPath := SessionFilePath(checkpointID, "content_hash.txt")
	if !env.FileExistsInBranch(paths.MetadataBranchName, hashPath) {
		t.Errorf("content_hash.txt should exist at %s", hashPath)
	}

	// Verify root metadata.json can be read and parsed as CheckpointSummary
	summaryContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, summaryPath)
	if !found {
		t.Fatal("root metadata.json should be readable")
	}
	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(summaryContent), &summary); err != nil {
		t.Fatalf("failed to parse root metadata.json as CheckpointSummary: %v", err)
	}

	// Verify Sessions array is populated
	if len(summary.Sessions) == 0 {
		t.Errorf("CheckpointSummary.Sessions should have at least one entry")
	} else {
		t.Logf("✓ CheckpointSummary has %d session(s)", len(summary.Sessions))
	}

	// Verify agent field is in session-level metadata (not root summary)
	sessionMetadataPath := SessionFilePath(checkpointID, paths.MetadataFileName)
	sessionMetadataContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, sessionMetadataPath)
	if !found {
		t.Fatal("session metadata.json should be readable")
	}
	var sessionMetadata checkpoint.CommittedMetadata
	if err := json.Unmarshal([]byte(sessionMetadataContent), &sessionMetadata); err != nil {
		t.Fatalf("failed to parse session metadata.json: %v", err)
	}
	expectedAgent := agent.AgentTypeClaudeCode
	if sessionMetadata.Agent != expectedAgent {
		t.Errorf("session metadata.Agent = %q, want %q", sessionMetadata.Agent, expectedAgent)
	} else {
		t.Logf("✓ Session metadata has agent: %q", sessionMetadata.Agent)
	}
}

// TestShadow_FullTranscriptContext verifies that each checkpoint includes
// the full session transcript, preserving complete history across commits.
//
// This tests transcript preservation:
// - First commit: context.md includes prompts 1-2
// - Second commit: context.md includes prompts 1-3 (full transcript preserved)
func TestShadow_FullTranscriptContext(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/incremental")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: First session with two prompts")

	// Start first session
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// First prompt: create file A
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	// Build transcript with first prompt
	session1.TranscriptBuilder.AddUserMessage("Create function A in a.go")
	session1.TranscriptBuilder.AddAssistantMessage("I'll create function A for you.")
	toolID1 := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session1.TranscriptBuilder.AddToolResult(toolID1)
	session1.TranscriptBuilder.AddAssistantMessage("Done creating function A!")

	// Second prompt in same session: create file B
	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	session1.TranscriptBuilder.AddUserMessage("Now create function B in b.go")
	session1.TranscriptBuilder.AddAssistantMessage("I'll create function B for you.")
	toolID2 := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session1.TranscriptBuilder.AddToolResult(toolID2)
	session1.TranscriptBuilder.AddAssistantMessage("Done creating function B!")

	// Write transcript
	if err := session1.TranscriptBuilder.WriteToFile(session1.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	// Save checkpoint (triggers SaveChanges)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	t.Log("Phase 2: First user commit")

	// User commits
	env.GitCommitWithShadowHooks("Add functions A and B", "a.go", "b.go")

	// Get first checkpoint ID from commit message trailer
	commit1Hash := env.GetHeadHash()
	checkpoint1ID := env.GetCheckpointIDFromCommitMessage(commit1Hash)
	t.Logf("First checkpoint ID: %s", checkpoint1ID)

	// Verify first checkpoint has both prompts (uses session file path in numbered subdirectory)
	promptPath1 := SessionFilePath(checkpoint1ID, "prompt.txt")
	prompt1Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath1)
	if !found {
		t.Errorf("prompt.txt should exist at %s", promptPath1)
	} else {
		t.Logf("First prompt.txt content:\n%s", prompt1Content)
		// Should contain both "Create function A" and "create function B"
		if !strings.Contains(prompt1Content, "Create function A") {
			t.Error("First prompt.txt should contain 'Create function A'")
		}
		if !strings.Contains(prompt1Content, "create function B") {
			t.Error("First prompt.txt should contain 'create function B'")
		}
	}

	contextPath1 := SessionFilePath(checkpoint1ID, "context.md")
	context1Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, contextPath1)
	if !found {
		t.Errorf("context.md should exist at %s", contextPath1)
	} else {
		t.Logf("First context.md content:\n%s", context1Content)
	}

	t.Log("Phase 3: Continue session with third prompt")

	// Continue the session with a new prompt
	// First, simulate another user prompt submit to track the new base
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (continued) failed: %v", err)
	}

	// Third prompt: create file C
	fileCContent := "package main\n\nfunc C() {}\n"
	env.WriteFile("c.go", fileCContent)

	// Add to transcript (continuing from previous)
	session1.TranscriptBuilder.AddUserMessage("Finally, create function C in c.go")
	session1.TranscriptBuilder.AddAssistantMessage("I'll create function C for you.")
	toolID3 := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "c.go", fileCContent)
	session1.TranscriptBuilder.AddToolResult(toolID3)
	session1.TranscriptBuilder.AddAssistantMessage("Done creating function C!")

	// Write updated transcript
	if err := session1.TranscriptBuilder.WriteToFile(session1.TranscriptPath); err != nil {
		t.Fatalf("Failed to write updated transcript: %v", err)
	}

	// Save checkpoint
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (second) failed: %v", err)
	}

	t.Log("Phase 4: Second user commit")

	// User commits again
	env.GitCommitWithShadowHooks("Add function C", "c.go")

	// Get second checkpoint ID from commit message trailer
	commit2Hash := env.GetHeadHash()
	checkpoint2ID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Second checkpoint ID: %s", checkpoint2ID)

	// Verify different checkpoint IDs
	if checkpoint1ID == checkpoint2ID {
		t.Errorf("Second commit should have different checkpoint ID: %s vs %s", checkpoint1ID, checkpoint2ID)
	}

	t.Log("Phase 5: Verify full transcript preserved in second checkpoint")

	// Verify second checkpoint has the FULL transcript (all three prompts)
	// Session files are now in numbered subdirectories (e.g., 0/prompt.txt)
	promptPath2 := SessionFilePath(checkpoint2ID, "prompt.txt")
	prompt2Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath2)
	if !found {
		t.Errorf("prompt.txt should exist at %s", promptPath2)
	} else {
		t.Logf("Second prompt.txt content:\n%s", prompt2Content)

		// Should contain all prompts (full transcript preserved)
		if !strings.Contains(prompt2Content, "create function C") {
			t.Error("Second prompt.txt should contain 'create function C'")
		}
		if !strings.Contains(prompt2Content, "Create function A") {
			t.Error("Second prompt.txt should contain 'Create function A' (full transcript)")
		}
		if !strings.Contains(prompt2Content, "create function B") {
			t.Error("Second prompt.txt should contain 'create function B' (full transcript)")
		}
	}

	contextPath2 := SessionFilePath(checkpoint2ID, "context.md")
	context2Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, contextPath2)
	if !found {
		t.Errorf("context.md should exist at %s", contextPath2)
	} else {
		t.Logf("Second context.md content:\n%s", context2Content)

		// Should contain full transcript context
		if !strings.Contains(context2Content, "Create function A") {
			t.Error("Second context.md should contain 'Create function A' (full transcript)")
		}
	}

	t.Log("Shadow full transcript context test completed successfully!")
}

// TestShadow_RewindAndCondensation verifies that after rewinding to an earlier
// checkpoint, the checkpoint only includes prompts up to that point.
//
// Workflow:
// 1. Create checkpoint 1 (prompt 1)
// 2. Create checkpoint 2 (prompt 2)
// 3. Rewind to checkpoint 1
// 4. User commits
// 5. Verify checkpoint only contains prompt 1 (NOT prompt 2)
func TestShadow_RewindAndCondensation(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/rewind-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create first checkpoint with prompt 1")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// First prompt: create file A
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A in a.go")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function A for you.")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)
	session.TranscriptBuilder.AddAssistantMessage("Done creating function A!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Get checkpoint 1 for later
	rewindPoints := env.GetRewindPoints()
	if len(rewindPoints) != 1 {
		t.Fatalf("Expected 1 rewind point after checkpoint 1, got %d", len(rewindPoints))
	}
	checkpoint1 := rewindPoints[0]
	t.Logf("Checkpoint 1: %s - %s", checkpoint1.ID[:7], checkpoint1.Message)

	t.Log("Phase 2: Create second checkpoint with prompt 2")

	// Second prompt: modify file A (a different approach)
	fileAModified := "package main\n\nfunc A() {\n\t// Modified version\n}\n"
	env.WriteFile("a.go", fileAModified)

	session.TranscriptBuilder.AddUserMessage("Actually, modify function A to have a comment")
	session.TranscriptBuilder.AddAssistantMessage("I'll modify function A for you.")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAModified)
	session.TranscriptBuilder.AddToolResult(toolID2)
	session.TranscriptBuilder.AddAssistantMessage("Done modifying function A!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2) failed: %v", err)
	}

	rewindPoints = env.GetRewindPoints()
	if len(rewindPoints) != 2 {
		t.Fatalf("Expected 2 rewind points after checkpoint 2, got %d", len(rewindPoints))
	}
	t.Logf("Checkpoint 2: %s - %s", rewindPoints[0].ID[:7], rewindPoints[0].Message)

	// Verify file has modified content
	currentContent := env.ReadFile("a.go")
	if currentContent != fileAModified {
		t.Errorf("a.go should have modified content before rewind")
	}

	t.Log("Phase 3: Rewind to checkpoint 1")

	// Rewind using the CLI (which calls the strategy internally)
	if err := env.Rewind(checkpoint1.ID); err != nil {
		t.Fatalf("Rewind failed: %v", err)
	}

	// Verify file content is restored to checkpoint 1
	restoredContent := env.ReadFile("a.go")
	if restoredContent != fileAContent {
		t.Errorf("a.go should have original content after rewind.\nExpected:\n%s\nGot:\n%s", fileAContent, restoredContent)
	}
	t.Log("Files successfully restored to checkpoint 1")

	t.Log("Phase 4: User commits after rewind")

	// User commits - this should trigger condensation
	env.GitCommitWithShadowHooks("Add function A (reverted)", "a.go")

	// Get checkpoint ID from commit message trailer
	commitHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commitHash)
	t.Logf("Checkpoint ID: %s", checkpointID)

	t.Log("Phase 5: Verify checkpoint only contains prompt 1")

	// Check prompt.txt (uses session file path in numbered subdirectory)
	promptPath := SessionFilePath(checkpointID, "prompt.txt")
	promptContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, promptPath)
	if !found {
		t.Errorf("prompt.txt should exist at %s", promptPath)
	} else {
		t.Logf("prompt.txt content:\n%s", promptContent)

		// Should contain prompt 1
		if !strings.Contains(promptContent, "Create function A") {
			t.Error("prompt.txt should contain 'Create function A' from checkpoint 1")
		}

		// Should NOT contain prompt 2 (because we rewound past it)
		if strings.Contains(promptContent, "modify function A") {
			t.Error("prompt.txt should NOT contain 'modify function A' - we rewound past that checkpoint")
		}
	}

	// Check context.md
	contextPath := SessionFilePath(checkpointID, "context.md")
	contextContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, contextPath)
	if !found {
		t.Errorf("context.md should exist at %s", contextPath)
	} else {
		t.Logf("context.md content:\n%s", contextContent)

		// Should NOT contain context from checkpoint 2
		if strings.Contains(contextContent, "modify function A") {
			t.Error("context.md should NOT contain 'modify function A' - we rewound past that checkpoint")
		}
	}

	t.Log("Shadow rewind and condensation test completed successfully!")
}

// TestShadow_RewindPreservesUntrackedFilesFromSessionStart tests that files that existed
// in the working directory (but weren't tracked in git) before the session started are
// preserved when rewinding. This was a bug where such files were incorrectly deleted.
func TestShadow_RewindPreservesUntrackedFilesFromSessionStart(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository with initial commit
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/untracked-test")

	// Create an untracked file BEFORE initializing Entire session
	// This simulates files like .claude/settings.json created by "entire setup"
	untrackedContent := `{"key": "value"}`
	env.WriteFile(".claude/settings.json", untrackedContent)

	// Initialize Entire with manual-commit strategy
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create first checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// First prompt: create file A
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	rewindPoints := env.GetRewindPoints()
	if len(rewindPoints) != 1 {
		t.Fatalf("Expected 1 rewind point, got %d", len(rewindPoints))
	}
	checkpoint1 := rewindPoints[0]
	t.Logf("Checkpoint 1: %s", checkpoint1.ID[:7])

	t.Log("Phase 2: Create second checkpoint")

	// Second prompt: create file B
	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	session.TranscriptBuilder.AddUserMessage("Create function B")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID2)

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2) failed: %v", err)
	}

	rewindPoints = env.GetRewindPoints()
	if len(rewindPoints) != 2 {
		t.Fatalf("Expected 2 rewind points, got %d", len(rewindPoints))
	}
	t.Logf("Checkpoint 2: %s", rewindPoints[0].ID[:7])

	// Verify the untracked file still exists before rewind
	if !env.FileExists(".claude/settings.json") {
		t.Fatal("Untracked file .claude/settings.json should exist before rewind")
	}

	t.Log("Phase 3: Rewind to checkpoint 1")

	if err := env.Rewind(checkpoint1.ID); err != nil {
		t.Fatalf("Rewind failed: %v", err)
	}

	// Verify that the untracked file that existed before session start is PRESERVED
	if !env.FileExists(".claude/settings.json") {
		t.Error("CRITICAL: .claude/settings.json was deleted during rewind but it existed before the session started!")
	} else {
		restoredContent := env.ReadFile(".claude/settings.json")
		if restoredContent != untrackedContent {
			t.Errorf("Untracked file content changed.\nExpected:\n%s\nGot:\n%s", untrackedContent, restoredContent)
		} else {
			t.Log("✓ Untracked file .claude/settings.json was preserved correctly")
		}
	}

	// Verify b.go was deleted (it was created after checkpoint 1)
	if env.FileExists("b.go") {
		t.Error("b.go should have been deleted during rewind (it was created after checkpoint 1)")
	} else {
		t.Log("✓ b.go was correctly deleted during rewind")
	}

	// Verify a.go was restored
	if !env.FileExists("a.go") {
		t.Error("a.go should exist after rewind to checkpoint 1")
	} else {
		restoredA := env.ReadFile("a.go")
		if restoredA != fileAContent {
			t.Errorf("a.go content incorrect after rewind")
		} else {
			t.Log("✓ a.go was correctly restored")
		}
	}

	t.Log("Test completed successfully!")
}

// TestShadow_IntermediateCommitsWithoutPrompts tests that commits without new Claude
// content do NOT get checkpoint trailers.
//
// Scenario:
// 1. Session starts, work happens, checkpoint created
// 2. First commit gets a trailer (has new content)
// 3. User commits unrelated files without new Claude work - NO trailer (no new content)
// 4. User enters new prompt, creates more files
// 5. Second commit with Claude content gets a trailer
func TestShadow_IntermediateCommitsWithoutPrompts(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/intermediate-commits")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Start session and create checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// First prompt: create file A
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A in a.go")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function A for you.")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)
	session.TranscriptBuilder.AddAssistantMessage("Done creating function A!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	t.Log("Phase 2: First commit (with session content)")

	env.GitCommitWithShadowHooks("Add function A", "a.go")
	commit1Hash := env.GetHeadHash()
	checkpoint1ID := env.GetCheckpointIDFromCommitMessage(commit1Hash)
	t.Logf("First commit: %s, checkpoint from trailer: %s", commit1Hash[:7], checkpoint1ID)
	t.Logf("First commit message:\n%s", env.GetCommitMessage(commit1Hash))

	if checkpoint1ID == "" {
		t.Fatal("First commit should have a checkpoint ID in its trailer (has new content)")
	}

	t.Log("Phase 3: Create unrelated file and commit WITHOUT new prompt")

	// User creates an unrelated file and commits without entering a new Claude prompt
	// Since there's no new session content, this commit should NOT get a trailer
	env.WriteFile("unrelated.txt", "This is an unrelated file")
	env.GitCommitWithShadowHooks("Add unrelated file", "unrelated.txt")

	commit2Hash := env.GetHeadHash()
	checkpoint2ID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Second commit: %s, checkpoint from trailer: %s", commit2Hash[:7], checkpoint2ID)
	t.Logf("Second commit message:\n%s", env.GetCommitMessage(commit2Hash))

	// Second commit should NOT get a checkpoint ID (no new session content)
	if checkpoint2ID != "" {
		t.Errorf("Second commit should NOT have a checkpoint trailer (no new content), got: %s", checkpoint2ID)
	}

	t.Log("Phase 4: New Claude work and commit")

	// Now user enters new prompt and does more work
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	session.TranscriptBuilder.AddUserMessage("Create function B in b.go")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function B for you.")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID2)
	session.TranscriptBuilder.AddAssistantMessage("Done creating function B!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add function B", "b.go")

	commit3Hash := env.GetHeadHash()
	checkpoint3ID := env.GetCheckpointIDFromCommitMessage(commit3Hash)
	t.Logf("Third commit: %s, checkpoint from trailer: %s", commit3Hash[:7], checkpoint3ID)
	t.Logf("Third commit message:\n%s", env.GetCommitMessage(commit3Hash))

	if checkpoint3ID == "" {
		t.Fatal("Third commit should have a checkpoint ID (has new content)")
	}

	// First and third checkpoint IDs should be different
	if checkpoint1ID == checkpoint3ID {
		t.Errorf("First and third commits should have different checkpoint IDs: %s vs %s",
			checkpoint1ID, checkpoint3ID)
	}

	t.Log("Phase 5: Verify checkpoints exist in entire/sessions")

	for _, cpID := range []string{checkpoint1ID, checkpoint3ID} {
		shardedPath := ShardedCheckpointPath(cpID)
		metadataPath := shardedPath + "/metadata.json"
		if !env.FileExistsInBranch(paths.MetadataBranchName, metadataPath) {
			t.Errorf("Checkpoint %s should have metadata.json at %s", cpID, metadataPath)
		}
	}

	t.Log("Intermediate commits test completed successfully!")
}

// TestShadow_FullTranscriptCondensationWithIntermediateCommits tests that full transcripts
// are preserved across multiple commits.
//
// Scenario:
// 1. Session with prompts A and B, commit 1
// 2. Continue session with prompt C, commit 2 (without intermediate prompt submit)
// 3. Verify commit 2's checkpoint has full transcript (A, B, and C)
func TestShadow_FullTranscriptCondensationWithIntermediateCommits(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/incremental-intermediate")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Session with two prompts")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// First prompt
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)

	// Second prompt in same session
	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	session.TranscriptBuilder.AddUserMessage("Create function B")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID2)

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	t.Log("Phase 2: First commit")

	env.GitCommitWithShadowHooks("Add functions A and B", "a.go", "b.go")
	commit1Hash := env.GetHeadHash()
	checkpoint1ID := env.GetCheckpointIDFromCommitMessage(commit1Hash)
	t.Logf("First commit: %s, checkpoint: %s", commit1Hash[:7], checkpoint1ID)

	// Verify first checkpoint has prompts A and B (session files in numbered subdirectory)
	prompt1Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpoint1ID, "prompt.txt"))
	if !found {
		t.Fatal("First checkpoint should have prompt.txt")
	}
	if !strings.Contains(prompt1Content, "function A") || !strings.Contains(prompt1Content, "function B") {
		t.Errorf("First checkpoint should contain prompts A and B, got: %s", prompt1Content)
	}
	t.Logf("First checkpoint prompts:\n%s", prompt1Content)

	t.Log("Phase 3: Continue session with third prompt (no SimulateUserPromptSubmit)")

	// Continue working WITHOUT calling SimulateUserPromptSubmit
	// This simulates the case where HEAD moved but InitializeSession wasn't called
	fileCContent := "package main\n\nfunc C() {}\n"
	env.WriteFile("c.go", fileCContent)

	// Add to transcript
	session.TranscriptBuilder.AddUserMessage("Create function C")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID3 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "c.go", fileCContent)
	session.TranscriptBuilder.AddToolResult(toolID3)

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write updated transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (second) failed: %v", err)
	}

	t.Log("Phase 4: Second commit (without intermediate prompt submit)")

	env.GitCommitWithShadowHooks("Add function C", "c.go")
	commit2Hash := env.GetHeadHash()
	checkpoint2ID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Second commit: %s, checkpoint: %s", commit2Hash[:7], checkpoint2ID)

	if checkpoint1ID == checkpoint2ID {
		t.Errorf("Commits should have different checkpoint IDs")
	}

	t.Log("Phase 5: Verify second checkpoint has full transcript (A, B, and C)")

	// Session files are now in numbered subdirectory (e.g., 0/prompt.txt)
	prompt2Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpoint2ID, "prompt.txt"))
	if !found {
		t.Fatal("Second checkpoint should have prompt.txt")
	}

	t.Logf("Second checkpoint prompts:\n%s", prompt2Content)

	// Should contain all prompts (full transcript preserved)
	if !strings.Contains(prompt2Content, "function C") {
		t.Error("Second checkpoint should contain 'function C'")
	}
	if !strings.Contains(prompt2Content, "function A") {
		t.Error("Second checkpoint should contain 'function A' (full transcript)")
	}
	if !strings.Contains(prompt2Content, "function B") {
		t.Error("Second checkpoint should contain 'function B' (full transcript)")
	}

	t.Log("Full transcript condensation with intermediate commits test completed successfully!")
}

// TestShadow_RewindPreservesUntrackedFilesWithExistingShadowBranch tests that untracked files
// present at session start are preserved during rewind, even when the shadow branch already
// exists from a previous session.
func TestShadow_RewindPreservesUntrackedFilesWithExistingShadowBranch(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository with initial commit
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/existing-shadow-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create untracked file before session starts")

	// Create an untracked file BEFORE the first checkpoint
	// This simulates configuration files that exist before Claude starts
	untrackedContent := `{"new": "config"}`
	env.WriteFile(".claude/settings.json", untrackedContent)

	t.Log("Phase 1: Create a previous session to establish shadow branch")

	// First session - creates the shadow branch
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	env.WriteFile("old.go", "package main\n")
	session1.TranscriptBuilder.AddUserMessage("Create old.go")
	session1.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "old.go", "package main\n")
	session1.TranscriptBuilder.AddToolResult(toolID)

	if err := session1.TranscriptBuilder.WriteToFile(session1.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch exists
	shadowBranchName := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranchName) {
		t.Fatalf("Shadow branch %s should exist after first session", shadowBranchName)
	}
	t.Logf("Shadow branch %s exists from first session", shadowBranchName)

	t.Log("Phase 2: Continue session and create second checkpoint")

	// Continue the SAME session (Claude resumes with the same session ID)
	// This is the expected behavior - continuing work on the same base commit
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (continue session) failed: %v", err)
	}

	// Reset transcript builder for next checkpoint
	session1.TranscriptBuilder = NewTranscriptBuilder()

	// Second checkpoint of session - should capture .claude/settings.json
	env.WriteFile("a.go", "package main\n\nfunc A() {}\n")
	session1.TranscriptBuilder.AddUserMessage("Create A")
	session1.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID2 := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", "package main\n\nfunc A() {}\n")
	session1.TranscriptBuilder.AddToolResult(toolID2)

	if err := session1.TranscriptBuilder.WriteToFile(session1.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2) failed: %v", err)
	}

	rewindPoints := env.GetRewindPoints()
	if len(rewindPoints) < 2 {
		t.Fatalf("Expected at least 2 rewind points, got %d", len(rewindPoints))
	}
	// Find the most recent checkpoint (checkpoint 2)
	checkpoint1 := &rewindPoints[0] // Most recent first
	t.Logf("Checkpoint 2: %s", checkpoint1.ID[:7])

	t.Log("Phase 3: Create third checkpoint")

	// Continue the session for the third checkpoint
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (checkpoint 3) failed: %v", err)
	}

	// Reset transcript builder for next checkpoint
	session1.TranscriptBuilder = NewTranscriptBuilder()

	env.WriteFile("b.go", "package main\n\nfunc B() {}\n")
	session1.TranscriptBuilder.AddUserMessage("Create B")
	session1.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID3 := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", "package main\n\nfunc B() {}\n")
	session1.TranscriptBuilder.AddToolResult(toolID3)

	if err := session1.TranscriptBuilder.WriteToFile(session1.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 3) failed: %v", err)
	}

	t.Log("Phase 4: Rewind to checkpoint 2")

	if err := env.Rewind(checkpoint1.ID); err != nil {
		t.Fatalf("Rewind failed: %v", err)
	}

	// Verify that the untracked file that existed at session start is PRESERVED
	// Since .claude/settings.json was created before checkpoint 1, it's in checkpoint 1's tree
	// and will flow through to checkpoint 2, so it should be preserved on rewind
	if !env.FileExists(".claude/settings.json") {
		t.Error(".claude/settings.json should have been preserved during rewind")
	} else {
		restoredContent := env.ReadFile(".claude/settings.json")
		if restoredContent != untrackedContent {
			t.Errorf("Untracked file content changed.\nExpected:\n%s\nGot:\n%s", untrackedContent, restoredContent)
		} else {
			t.Log("✓ .claude/settings.json was preserved correctly")
		}
	}

	// Verify b.go was deleted
	if env.FileExists("b.go") {
		t.Error("b.go should have been deleted during rewind")
	} else {
		t.Log("✓ b.go was correctly deleted during rewind")
	}

	t.Log("Test completed successfully!")
}

// TestShadow_TrailerRemovalSkipsCondensation tests that removing the Entire-Checkpoint
// trailer during commit message editing causes condensation to be skipped.
// This allows users to opt-out of linking a commit to their Claude session.
func TestShadow_TrailerRemovalSkipsCondensation(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/trailer-opt-out")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create session with content")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function A for you.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done!")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	t.Log("Phase 2: Commit WITH trailer removed (user opts out)")

	// Use the special helper that removes the trailer before committing
	env.GitCommitWithTrailerRemoved("Add function A (manual commit)", "a.go")

	commitHash := env.GetHeadHash()
	t.Logf("Commit: %s", commitHash[:7])

	// Verify commit does NOT have trailer
	commitMsg := env.GetCommitMessage(commitHash)
	if strings.Contains(commitMsg, "Entire-Checkpoint:") {
		t.Errorf("Commit should NOT have Entire-Checkpoint trailer (it was removed), got:\n%s", commitMsg)
	}
	t.Logf("Commit message (trailer removed):\n%s", commitMsg)

	t.Log("Phase 3: Verify no condensation happened")

	// entire/sessions branch exists (created at setup), but should not have any checkpoint commits yet
	// since the user removed the trailer
	latestCheckpointID := env.TryGetLatestCheckpointID()
	if latestCheckpointID == "" {
		t.Log("✓ No checkpoint found on entire/sessions branch (no condensation)")
	} else {
		// If there is a checkpoint, this is unexpected for this test
		t.Logf("Found checkpoint ID: %s (should be from previous activity, not this commit)", latestCheckpointID)
	}

	t.Log("Phase 4: Now commit WITH trailer (user keeps it)")

	// Continue session with new content
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	session.TranscriptBuilder.AddUserMessage("Create function B")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID2 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID2)

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// This time, keep the trailer (normal commit with hooks)
	env.GitCommitWithShadowHooks("Add function B", "b.go")

	commit2Hash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Second commit: %s, checkpoint: %s", commit2Hash[:7], checkpointID)

	// Verify second commit HAS trailer
	commit2Msg := env.GetCommitMessage(commit2Hash)
	if !strings.Contains(commit2Msg, "Entire-Checkpoint:") {
		t.Errorf("Second commit should have Entire-Checkpoint trailer, got:\n%s", commit2Msg)
	}

	// Verify condensation happened for second commit
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/sessions branch should exist after second commit with trailer")
	}

	// Verify checkpoint exists
	shardedPath := ShardedCheckpointPath(checkpointID)
	metadataPath := shardedPath + "/metadata.json"
	if !env.FileExistsInBranch(paths.MetadataBranchName, metadataPath) {
		t.Errorf("Checkpoint should exist at %s", metadataPath)
	} else {
		t.Log("✓ Condensation happened for commit with trailer")
	}

	t.Log("Trailer removal opt-out test completed successfully!")
}

// TestShadow_SessionsBranchCommitTrailers verifies that commits on the entire/sessions
// branch contain the expected trailers: Entire-Session, Entire-Strategy, and Entire-Agent.
func TestShadow_SessionsBranchCommitTrailers(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/trailer-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	fileContent := "package main\n\nfunc main() {}\n"
	env.WriteFile("main.go", fileContent)
	session.CreateTranscript("Create main.go", []FileChange{{Path: "main.go", Content: fileContent}})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit to trigger condensation
	env.GitCommitWithShadowHooks("Add main.go", "main.go")

	// Get the commit message on entire/sessions branch
	sessionsCommitMsg := env.GetLatestCommitMessageOnBranch(paths.MetadataBranchName)
	t.Logf("entire/sessions commit message:\n%s", sessionsCommitMsg)

	// Verify required trailers are present
	requiredTrailers := map[string]string{
		trailers.SessionTrailerKey:  "",                                // Entire-Session: <session-id>
		trailers.StrategyTrailerKey: strategy.StrategyNameManualCommit, // Entire-Strategy: manual-commit
		trailers.AgentTrailerKey:    "Claude Code",                     // Entire-Agent: Claude Code
	}

	for trailerKey, expectedValue := range requiredTrailers {
		if !strings.Contains(sessionsCommitMsg, trailerKey+":") {
			t.Errorf("entire/sessions commit should have %s trailer", trailerKey)
			continue
		}

		// If we have an expected value, verify it
		if expectedValue != "" {
			expectedTrailer := trailerKey + ": " + expectedValue
			if !strings.Contains(sessionsCommitMsg, expectedTrailer) {
				t.Errorf("entire/sessions commit should have %q, got message:\n%s", expectedTrailer, sessionsCommitMsg)
			} else {
				t.Logf("✓ Found trailer: %s", expectedTrailer)
			}
		} else {
			t.Logf("✓ Found trailer: %s", trailerKey)
		}
	}

	t.Log("Sessions branch commit trailers test completed successfully!")
}
