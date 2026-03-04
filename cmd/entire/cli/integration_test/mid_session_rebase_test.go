//go:build integration

package integration

import (
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestShadow_MidSessionRebaseMigration tests that when Claude performs a rebase
// mid-session (via a tool call), the shadow branch is automatically migrated
// to the new HEAD and subsequent checkpoints are saved correctly.
//
// This is a critical scenario because:
// 1. Claude can run `git rebase` via the Bash tool
// 2. No new prompt is submitted between the rebase and the next checkpoint
// 3. Without migration in SaveStep, checkpoints go to an orphaned shadow branch
//
// The test validates that:
// - Checkpoints before rebase go to the original shadow branch
// - Checkpoints after rebase go to the new (migrated) shadow branch
// - The session state's BaseCommit is updated correctly
func TestShadow_MidSessionRebaseMigration(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// ========================================
	// Phase 1: Setup - Create commits to rebase onto
	// ========================================
	env.InitRepo()

	// Create initial commit on main
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create a second commit on main (this will be our rebase target)
	env.WriteFile("base.txt", "base content")
	env.GitAdd("base.txt")
	env.GitCommit("Add base file")
	mainHead := env.GetHeadHash()

	// Create feature branch from initial commit (before base.txt)
	env.gitCheckout("HEAD~1")
	env.GitCheckoutNewBranch("feature/rebase-test")

	// Initialize Entire after branch creation
	env.InitEntire()

	// Create a commit on feature branch
	env.WriteFile("feature.txt", "feature content")
	env.GitAdd("feature.txt")
	env.GitCommit("Add feature file")

	initialFeatureHead := env.GetHeadHash()
	t.Logf("Initial feature HEAD: %s", initialFeatureHead[:7])
	t.Logf("Main HEAD (rebase target): %s", mainHead[:7])

	// ========================================
	// Phase 2: Start session and create first checkpoint
	// ========================================
	t.Log("Phase 2: Starting session and creating first checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create first file change
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.CreateTranscript(
		"Create function A",
		[]FileChange{{Path: "a.go", Content: fileAContent}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Verify shadow branch exists at original commit
	originalShadowBranch := env.GetShadowBranchNameForCommit(initialFeatureHead)
	if !env.BranchExists(originalShadowBranch) {
		t.Fatalf("Shadow branch %s should exist after first checkpoint", originalShadowBranch)
	}
	t.Logf("Checkpoint 1 created on shadow branch: %s", originalShadowBranch)

	// Verify 1 rewind point
	points := env.GetRewindPoints()
	if len(points) != 1 {
		t.Fatalf("Expected 1 rewind point after first checkpoint, got %d", len(points))
	}

	// ========================================
	// Phase 3: Simulate Claude doing a rebase (via Bash tool)
	// ========================================
	t.Log("Phase 3: Simulating Claude performing rebase via Bash tool")

	// This simulates what happens when Claude runs: git rebase master
	// Note: We're NOT calling SimulateUserPromptSubmit here because the rebase
	// happens mid-session as part of Claude's tool execution
	cmd := exec.Command("git", "rebase", "master")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git rebase failed: %v\nOutput: %s", err, output)
	}

	newFeatureHead := env.GetHeadHash()
	t.Logf("After rebase, feature HEAD: %s (was: %s)", newFeatureHead[:7], initialFeatureHead[:7])

	// Verify HEAD actually changed (rebase happened)
	if newFeatureHead == initialFeatureHead {
		t.Fatal("HEAD should have changed after rebase")
	}

	// ========================================
	// Phase 4: Create second checkpoint AFTER rebase (without new prompt)
	// ========================================
	t.Log("Phase 4: Creating checkpoint after rebase (no new prompt submit)")

	// Claude continues working after the rebase - creates more files
	// Note: We do NOT call SimulateUserPromptSubmit because this is continuing
	// the same tool execution flow (no new user prompt)
	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	// Reset transcript builder for new checkpoint
	session.TranscriptBuilder = NewTranscriptBuilder()
	session.CreateTranscript(
		"Create function B after rebase",
		[]FileChange{{Path: "b.go", Content: fileBContent}},
	)

	// This is the critical test: SimulateStop calls SaveStep which should
	// detect HEAD has changed and migrate the shadow branch
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2 after rebase) failed: %v", err)
	}

	// ========================================
	// Phase 5: Verify migration happened
	// ========================================
	t.Log("Phase 5: Verifying shadow branch migration")

	// The new shadow branch should exist (based on rebased HEAD)
	newShadowBranch := env.GetShadowBranchNameForCommit(newFeatureHead)

	// Verify the new shadow branch exists
	if !env.BranchExists(newShadowBranch) {
		t.Errorf("New shadow branch %s should exist after migration", newShadowBranch)
		t.Logf("Available branches: %v", env.ListBranchesWithPrefix("entire/"))

		// Check if old shadow branch still exists (would indicate no migration)
		if env.BranchExists(originalShadowBranch) {
			t.Errorf("Original shadow branch %s still exists - migration did not happen!", originalShadowBranch)
		}
	} else {
		t.Logf("✓ New shadow branch exists: %s", newShadowBranch)
	}

	// Verify old shadow branch is gone (renamed/migrated)
	if env.BranchExists(originalShadowBranch) {
		t.Errorf("Original shadow branch %s should have been deleted after migration", originalShadowBranch)
	} else {
		t.Logf("✓ Original shadow branch %s was cleaned up", originalShadowBranch)
	}

	// Verify session state has updated BaseCommit
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist")
	}
	if state.BaseCommit != newFeatureHead {
		t.Errorf("Session BaseCommit should be %s (rebased HEAD), got %s",
			newFeatureHead[:7], state.BaseCommit[:7])
	} else {
		t.Logf("✓ Session BaseCommit updated to: %s", state.BaseCommit[:7])
	}

	// Verify we have 2 checkpoints (both should be on the new shadow branch)
	points = env.GetRewindPoints()
	if len(points) != 2 {
		t.Errorf("Expected 2 rewind points, got %d", len(points))
	} else {
		t.Logf("✓ Found %d rewind points after migration", len(points))
	}

	// ========================================
	// Phase 6: Verify rewind still works
	// ========================================
	t.Log("Phase 6: Verifying rewind still works after migration")

	// Find checkpoint 1 (before rebase)
	var checkpoint1ID string
	for _, p := range points {
		if p.Message == "Create function A" {
			checkpoint1ID = p.ID
			break
		}
	}
	if checkpoint1ID == "" {
		t.Fatalf("Could not find checkpoint 1 in rewind points")
	}

	// Rewind to checkpoint 1
	if err := env.Rewind(checkpoint1ID); err != nil {
		t.Fatalf("Rewind to checkpoint 1 failed: %v", err)
	}

	// Verify b.go is gone (it was created after checkpoint 1)
	if env.FileExists("b.go") {
		t.Error("b.go should NOT exist after rewind to checkpoint 1")
	}

	// Verify a.go exists
	if !env.FileExists("a.go") {
		t.Error("a.go should exist after rewind to checkpoint 1")
	}

	t.Log("Mid-session rebase migration test completed successfully!")
}

// gitCheckout is a helper to checkout a specific ref using git CLI.
// Uses CLI instead of go-git to work around go-git v5 bug with untracked files.
func (env *TestEnv) gitCheckout(ref string) {
	env.T.Helper()

	cmd := exec.Command("git", "checkout", ref)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git checkout %s failed: %v\nOutput: %s", ref, err, output)
	}
}

// TestShadow_CommitThenRebaseMidSession tests the scenario where Claude:
// 1. Creates checkpoints (shadow branch exists)
// 2. Commits the work (triggers condensation, shadow branch is DELETED)
// 3. Rebases onto another branch (HEAD changes)
// 4. Creates more checkpoints
//
// This verifies there's no race condition between shadow branch cleanup
// (from condensation) and the migration logic in SaveStep.
func TestShadow_CommitThenRebaseMidSession(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// ========================================
	// Phase 1: Setup - Create commits on master to rebase onto
	// ========================================
	env.InitRepo()

	// Create initial commit on master
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")

	// Create a second commit on master (rebase target)
	env.WriteFile("base.txt", "base content")
	env.GitAdd("base.txt")
	env.GitCommit("Add base file")

	// Create feature branch from initial commit
	env.gitCheckout("HEAD~1")
	env.GitCheckoutNewBranch("feature/commit-then-rebase")

	// Initialize Entire
	env.InitEntire()

	initialFeatureHead := env.GetHeadHash()
	t.Logf("Initial feature HEAD: %s", initialFeatureHead[:7])

	// ========================================
	// Phase 2: Start session and create first checkpoint
	// ========================================
	t.Log("Phase 2: Starting session and creating first checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create file and checkpoint
	fileAContent := "package main\n\nfunc A() {}\n"
	env.WriteFile("a.go", fileAContent)

	session.CreateTranscript(
		"Create function A",
		[]FileChange{{Path: "a.go", Content: fileAContent}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Verify shadow branch exists
	originalShadowBranch := env.GetShadowBranchNameForCommit(initialFeatureHead)
	if !env.BranchExists(originalShadowBranch) {
		t.Fatalf("Shadow branch %s should exist after first checkpoint", originalShadowBranch)
	}
	t.Logf("Checkpoint 1 created on shadow branch: %s", originalShadowBranch)

	// ========================================
	// Phase 3: Claude commits (triggers condensation which DELETES shadow branch)
	// ========================================
	t.Log("Phase 3: Claude commits (triggers condensation)")

	// Stage and commit the file
	env.GitAdd("a.go")
	// Use GitCommitWithShadowHooks to simulate the full commit flow with hooks
	env.GitCommitWithShadowHooks("Add function A", "a.go")

	postCommitHead := env.GetHeadHash()
	t.Logf("After commit, feature HEAD: %s", postCommitHead[:7])

	// Verify shadow branch was deleted by condensation
	if env.BranchExists(originalShadowBranch) {
		t.Logf("Note: Shadow branch %s still exists after commit (condensation may not have deleted it)", originalShadowBranch)
	} else {
		t.Logf("✓ Shadow branch %s was deleted by condensation", originalShadowBranch)
	}

	// Verify data was condensed to metadata branch
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatalf("%s branch should exist after condensation", paths.MetadataBranchName)
	}

	// ========================================
	// Phase 4: Claude rebases (HEAD changes again)
	// ========================================
	t.Log("Phase 4: Claude rebases onto master")

	cmd := exec.Command("git", "rebase", "master")
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git rebase failed: %v\nOutput: %s", err, output)
	}

	postRebaseHead := env.GetHeadHash()
	t.Logf("After rebase, feature HEAD: %s (was: %s)", postRebaseHead[:7], postCommitHead[:7])

	// Verify HEAD changed
	if postRebaseHead == postCommitHead {
		t.Fatal("HEAD should have changed after rebase")
	}

	// ========================================
	// Phase 5: Create another checkpoint after commit+rebase
	// ========================================
	t.Log("Phase 5: Creating checkpoint after commit and rebase")

	fileBContent := "package main\n\nfunc B() {}\n"
	env.WriteFile("b.go", fileBContent)

	// IMPORTANT: Don't reset the TranscriptBuilder - append to existing transcript
	// This simulates Claude continuing work in the same session after commit+rebase
	session.TranscriptBuilder.AddUserMessage("Now create function B")
	session.TranscriptBuilder.AddAssistantMessage("I'll create function B.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "b.go", fileBContent)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done creating function B!")

	// Write the updated transcript (with new content appended)
	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("failed to write transcript: %v", err)
	}

	// This should NOT fail even though:
	// - Original shadow branch was deleted by condensation
	// - HEAD changed twice (commit, then rebase)
	// - Session state still has old BaseCommit
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2 after commit+rebase) failed: %v", err)
	}

	// ========================================
	// Phase 6: Verify correct behavior
	// ========================================
	t.Log("Phase 6: Verifying correct behavior")

	// New shadow branch should exist (based on rebased HEAD)
	newShadowBranch := env.GetShadowBranchNameForCommit(postRebaseHead)
	if !env.BranchExists(newShadowBranch) {
		t.Errorf("New shadow branch %s should exist", newShadowBranch)
		t.Logf("Available branches: %v", env.ListBranchesWithPrefix("entire/"))
	} else {
		t.Logf("✓ New shadow branch exists: %s", newShadowBranch)
	}

	// Session state should have updated BaseCommit
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatal("Session state should exist")
	}
	if state.BaseCommit != postRebaseHead {
		t.Errorf("Session BaseCommit should be %s (rebased HEAD), got %s",
			postRebaseHead[:7], state.BaseCommit[:7])
	} else {
		t.Logf("✓ Session BaseCommit updated to: %s", state.BaseCommit[:7])
	}

	// Should have rewind points (at least the new checkpoint)
	points := env.GetRewindPoints()
	if len(points) == 0 {
		t.Error("Expected at least 1 rewind point")
	} else {
		t.Logf("✓ Found %d rewind point(s)", len(points))
	}

	t.Log("Commit-then-rebase mid-session test completed successfully!")
}
