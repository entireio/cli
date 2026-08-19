//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// TestShadow_FullWorkflow tests the complete shadow workflow as described in
// docs/requirements/shadow-strategy/example.md
//
// This test simulates Alice's workflow:
// 1. Start session, create checkpoints
// 2. User commits (triggers condensation)
// 3. Continue working after commit (new shadow branch)
// 4. User commits again (second condensation)
// 5. Verify final state
//
//nolint:maintidx // End-to-end workflow across 5 sequential phases; the shadow-branch assertions that replaced the rewind observations pushed it over the threshold, and the phases share so much accumulated state that splitting them would obscure the flow this test exists to document.
func TestShadow_FullWorkflow(t *testing.T) {
	t.Parallel()
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
	env.InitEntire()

	initialHead := env.GetHeadHash()
	t.Logf("Initial HEAD on feature/auth: %s", initialHead[:7])

	// ========================================
	// Phase 2: Session Start & First Checkpoint
	// ========================================
	t.Log("Phase 2: Starting session and creating first checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create authentication module"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
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

	// Verify checkpoint 1 content landed on the shadow branch
	if !env.FileExistsInBranch(expectedShadowBranch, "src/auth.go") {
		t.Error("src/auth.go should exist on shadow branch after first checkpoint")
	}
	t.Log("Checkpoint 1 created")

	// ========================================
	// Phase 3: Second Checkpoint (continuing same session)
	// ========================================
	t.Log("Phase 3: Creating second checkpoint (continuing same session)")

	// Continue the same session (not a new session) - this is the expected Claude behavior
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Add password hashing"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (checkpoint 2) failed: %v", err)
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

	// Verify checkpoint 2 content landed on the same shadow branch
	if !env.FileExistsInBranch(expectedShadowBranch, "src/hash.go") {
		t.Error("src/hash.go should exist on shadow branch after second checkpoint")
	}
	if branchContent, found := env.ReadFileFromBranch(expectedShadowBranch, "src/auth.go"); !found || branchContent != authV2 {
		t.Errorf("src/auth.go on shadow branch should have v2 content, got: %s", branchContent)
	}
	t.Log("Checkpoint 2 created")

	// Verify both files exist
	if !env.FileExists("src/hash.go") {
		t.Error("src/hash.go should exist after checkpoint 2")
	}
	if content := env.ReadFile("src/auth.go"); content != authV2 {
		t.Errorf("src/auth.go should have v2 content, got: %s", content)
	}

	// ========================================
	// Phase 4: Third Checkpoint (continue same session)
	// ========================================
	t.Log("Phase 4: Creating third checkpoint (continuing same session)")

	// Continue the same session
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Use bcrypt for hashing"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (checkpoint 3) failed: %v", err)
	}

	// Reset transcript builder for next checkpoint
	session.TranscriptBuilder = NewTranscriptBuilder()

	// Create bcrypt.go (different approach)
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

	// Verify checkpoint 3 content landed on the shadow branch
	if !env.FileExistsInBranch(expectedShadowBranch, "src/bcrypt.go") {
		t.Error("src/bcrypt.go should exist on shadow branch after third checkpoint")
	}

	// ========================================
	// Phase 5: User Commits (Condensation)
	// ========================================
	t.Log("Phase 5: User commits - triggering condensation")

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
	checkpoint1ID := env.GetLatestCheckpointIDFromHistory()
	t.Logf("Checkpoint 1 ID: %s", checkpoint1ID)

	// Verify entire/checkpoints/v1 branch exists with checkpoint folder
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Error("entire/checkpoints/v1 branch should exist after condensation")
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
	// Phase 6: Continue Working After Commit
	// ========================================
	t.Log("Phase 6: Continuing work after user commit")

	// Verify HEAD changed
	if commit1Hash == initialHead {
		t.Error("HEAD should have changed after user commit")
	}

	// Start new session
	session4 := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session4.ID, "Add session management"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (session4) failed: %v", err)
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
	// Phase 7: Second User Commit
	// ========================================
	t.Log("Phase 7: Second user commit")

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
	// Phase 8: Verify Final State
	// ========================================
	t.Log("Phase 8: Verifying final state")

	// 2 user commits on feature branch
	// Both should be clean (no Entire-* trailers)
	if strings.Contains(commitMsg, "Entire-Session:") || strings.Contains(commitMsg2, "Entire-Session:") {
		t.Error("Commits should NOT have Entire-Session trailer (clean history)")
	}

	// 2 checkpoint folders in entire/checkpoints/v1 (Already verified above)

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
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")

	// Initialize AFTER branch switch
	env.InitEntire()

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
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")

	// Initialize AFTER branch switch
	env.InitEntire()

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
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

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
	if state.StepCount != 2 {
		t.Errorf("Expected 2 checkpoints after migration, got %d", state.StepCount)
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
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	env.GitCheckoutNewBranch("feature/test")

	// Initialize AFTER branch switch
	env.InitEntire()

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
// included in the entire/checkpoints/v1 branch during condensation.
func TestShadow_TranscriptCondensation(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/test")
	env.InitEntire()

	// Start session and create checkpoint with transcript
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create main.go with hello world"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
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

	// Get checkpoint ID from entire/checkpoints/v1 branch (not from commit message)
	checkpointID := env.GetLatestCheckpointID()
	t.Logf("Checkpoint ID: %s", checkpointID)

	// Verify entire/checkpoints/v1 branch exists
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist after condensation")
	}

	// Comprehensive checkpoint validation
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:    checkpointID,
		SessionID:       session.ID,
		Strategy:        strategy.StrategyNameManualCommit,
		FilesTouched:    []string{"main.go"},
		ExpectedPrompts: []string{"Create main.go with hello world"},
		ExpectedTranscriptContent: []string{
			"Create main.go with hello world",
			"main.go",
		},
	})

	// Additionally verify agent field in session metadata
	sessionMetadataPath := SessionFilePath(checkpointID, paths.MetadataFileName)
	sessionMetadataContent, found := env.ReadFileFromBranch(paths.MetadataBranchName, sessionMetadataPath)
	if !found {
		t.Fatal("session metadata.json should be readable")
	}
	var sessionMetadata checkpoint.Metadata
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
// only the prompts from its checkpoint portion, not the entire session.
//
// This tests checkpoint-scoped prompts:
// - First commit: prompt.txt includes prompts 1-2 (from checkpoint start)
// - Second commit: prompt.txt includes only prompt 3 (from second checkpoint start)
func TestShadow_FullTranscriptContext(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/incremental")
	env.InitEntire()

	t.Log("Phase 1: First session with two prompts")

	// Start first session
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session1.ID, "Create function A in a.go"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	// First prompt: create file A
	fileAContent := pkgFuncA
	env.WriteFile("a.go", fileAContent)

	// Build transcript with first prompt
	session1.TranscriptBuilder.AddUserMessage("Create function A in a.go")
	session1.TranscriptBuilder.AddAssistantMessage("I'll create function A for you.")
	toolID1 := session1.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session1.TranscriptBuilder.AddToolResult(toolID1)
	session1.TranscriptBuilder.AddAssistantMessage("Done creating function A!")

	// Second prompt in same session: create file B
	if err := env.SimulateUserPromptSubmitWithPrompt(session1.ID, "Now create function B in b.go"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (second prompt) failed: %v", err)
	}
	fileBContent := pkgFuncB
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

	// Save checkpoint (triggers SaveStep)
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

	t.Log("Phase 3: Continue session with third prompt")

	// Continue the session with a new prompt
	// First, simulate another user prompt submit to track the new base
	if err := env.SimulateUserPromptSubmitWithPrompt(session1.ID, "Finally, create function C in c.go"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (continued) failed: %v", err)
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

		// Should contain only the checkpoint-scoped prompt (third prompt only)
		if !strings.Contains(prompt2Content, "create function C") {
			t.Error("Second prompt.txt should contain 'create function C'")
		}
	}

	t.Log("Shadow full transcript context test completed successfully!")
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
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/intermediate-commits")
	env.InitEntire()

	t.Log("Phase 1: Start session and create checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function A in a.go"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	// First prompt: create file A
	fileAContent := pkgFuncA
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
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function B in b.go"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	fileBContent := pkgFuncB
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

	t.Log("Phase 5: Verify checkpoints exist in entire/checkpoints/v1")

	for _, cpID := range []string{checkpoint1ID, checkpoint3ID} {
		shardedPath := ShardedCheckpointPath(cpID)
		metadataPath := shardedPath + "/metadata.json"
		if !env.FileExistsInBranch(paths.MetadataBranchName, metadataPath) {
			t.Errorf("Checkpoint %s should have metadata.json at %s", cpID, metadataPath)
		}
	}

	t.Log("Intermediate commits test completed successfully!")
}

// TestShadow_FullTranscriptCondensationWithIntermediateCommits tests that checkpoints
// contain only checkpoint-scoped prompts across multiple commits.
//
// Scenario:
// 1. Session with prompts A and B, commit 1 → prompt.txt has A and B
// 2. Continue session with prompt C, commit 2 (without intermediate prompt submit)
// 3. Verify commit 2's prompt.txt has only C (checkpoint-scoped)
func TestShadow_FullTranscriptCondensationWithIntermediateCommits(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup repository
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/incremental-intermediate")
	env.InitEntire()

	t.Log("Phase 1: Session with two prompts")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function A"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	// First prompt
	fileAContent := pkgFuncA
	env.WriteFile("a.go", fileAContent)

	session.TranscriptBuilder.AddUserMessage("Create function A")
	session.TranscriptBuilder.AddAssistantMessage("Done!")
	toolID1 := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "a.go", fileAContent)
	session.TranscriptBuilder.AddToolResult(toolID1)

	// Second prompt in same session
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function B"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (second prompt) failed: %v", err)
	}
	fileBContent := pkgFuncB
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

	t.Log("Phase 3: Continue session with third prompt")

	// Submit the new prompt through the hook so it gets recorded in prompt.txt
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function C"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt (third prompt) failed: %v", err)
	}

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

	t.Log("Phase 4: Second commit")

	env.GitCommitWithShadowHooks("Add function C", "c.go")
	commit2Hash := env.GetHeadHash()
	checkpoint2ID := env.GetCheckpointIDFromCommitMessage(commit2Hash)
	t.Logf("Second commit: %s, checkpoint: %s", commit2Hash[:7], checkpoint2ID)

	if checkpoint1ID == checkpoint2ID {
		t.Errorf("Commits should have different checkpoint IDs")
	}

	t.Log("Phase 5: Verify second checkpoint has only checkpoint-scoped prompt (C)")

	// Session files are now in numbered subdirectory (e.g., 0/prompt.txt)
	prompt2Content, found := env.ReadFileFromBranch(paths.MetadataBranchName, SessionFilePath(checkpoint2ID, "prompt.txt"))
	if !found {
		t.Fatal("Second checkpoint should have prompt.txt")
	}

	t.Logf("Second checkpoint prompts:\n%s", prompt2Content)

	// Should contain only the checkpoint-scoped prompt (C), not earlier prompts
	if !strings.Contains(prompt2Content, "function C") {
		t.Error("Second checkpoint should contain 'function C'")
	}
	if strings.Contains(prompt2Content, "function A") {
		t.Error("Second checkpoint should NOT contain 'function A' (checkpoint-scoped)")
	}
	if strings.Contains(prompt2Content, "function B") {
		t.Error("Second checkpoint should NOT contain 'function B' (checkpoint-scoped)")
	}

	t.Log("Checkpoint-scoped prompt condensation with intermediate commits test completed successfully!")
}

// TestShadow_TrailerRemovalSkipsCondensation tests that removing the Entire-Checkpoint
// trailer during commit message editing causes condensation to be skipped.
// This allows users to opt-out of linking a commit to their Claude session.
func TestShadow_TrailerRemovalSkipsCondensation(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/trailer-opt-out")
	env.InitEntire()

	t.Log("Phase 1: Create session with content")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function A"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	fileAContent := pkgFuncA
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
	if _, found := trailers.ParseCheckpoint(commitMsg); found {
		t.Errorf("Commit should NOT have Entire-Checkpoint trailer (it was removed), got:\n%s", commitMsg)
	}
	t.Logf("Commit message (trailer removed):\n%s", commitMsg)

	t.Log("Phase 3: Verify no condensation happened")

	// entire/checkpoints/v1 branch exists (created at setup), but should not have any checkpoint commits yet
	// since the user removed the trailer
	latestCheckpointID := env.TryGetLatestCheckpointID()
	if latestCheckpointID == "" {
		t.Log("✓ No checkpoint found on entire/checkpoints/v1 branch (no condensation)")
	} else {
		// If there is a checkpoint, this is unexpected for this test
		t.Logf("Found checkpoint ID: %s (should be from previous activity, not this commit)", latestCheckpointID)
	}

	t.Log("Phase 4: Now commit WITH trailer (user keeps it)")

	// Continue session with new content
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function B"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	fileBContent := pkgFuncB
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

	// Verify second commit HAS trailer with valid format
	commit2Msg := env.GetCommitMessage(commit2Hash)
	if _, found := trailers.ParseCheckpoint(commit2Msg); !found {
		t.Errorf("Second commit should have valid Entire-Checkpoint trailer, got:\n%s", commit2Msg)
	}

	// Verify condensation happened for second commit
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatal("entire/checkpoints/v1 branch should exist after second commit with trailer")
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

// TestShadow_SessionsBranchCommitTrailers verifies that commits on the entire/checkpoints/v1
// branch contain the expected trailers: Entire-Session, Entire-Strategy, and Entire-Agent.
func TestShadow_SessionsBranchCommitTrailers(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/trailer-test")
	env.InitEntire()

	// Start session and create checkpoint
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create main.go"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	fileContent := "package main\n\nfunc main() {}\n"
	env.WriteFile("main.go", fileContent)
	session.CreateTranscript("Create main.go", []FileChange{{Path: "main.go", Content: fileContent}})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Commit to trigger condensation
	env.GitCommitWithShadowHooks("Add main.go", "main.go")

	// Get the commit message on entire/checkpoints/v1 branch
	sessionsCommitMsg := env.GetLatestCommitMessageOnBranch(paths.MetadataBranchName)
	t.Logf("entire/checkpoints/v1 commit message:\n%s", sessionsCommitMsg)

	// Verify required trailers are present
	requiredTrailers := map[string]string{
		trailers.SessionTrailerKey:  "",                                // Entire-Session: <session-id>
		trailers.StrategyTrailerKey: strategy.StrategyNameManualCommit, // Entire-Strategy: manual-commit
		trailers.AgentTrailerKey:    "Claude Code",                     // Entire-Agent: Claude Code
	}

	for trailerKey, expectedValue := range requiredTrailers {
		if !strings.Contains(sessionsCommitMsg, trailerKey+":") {
			t.Errorf("entire/checkpoints/v1 commit should have %s trailer", trailerKey)
			continue
		}

		// If we have an expected value, verify it
		if expectedValue != "" {
			expectedTrailer := trailerKey + ": " + expectedValue
			if !strings.Contains(sessionsCommitMsg, expectedTrailer) {
				t.Errorf("entire/checkpoints/v1 commit should have %q, got message:\n%s", expectedTrailer, sessionsCommitMsg)
			} else {
				t.Logf("✓ Found trailer: %s", expectedTrailer)
			}
		} else {
			t.Logf("✓ Found trailer: %s", trailerKey)
		}
	}

	t.Log("Sessions branch commit trailers test completed successfully!")
}
