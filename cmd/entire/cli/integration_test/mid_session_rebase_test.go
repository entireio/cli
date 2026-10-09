//go:build integration

package integration

import (
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// TestManualCommit_MidSessionRebaseFollowsHead tests that when Claude performs a rebase
// mid-session (via a tool call), the session's BaseCommit follows HEAD at the
// next turn end and the earlier pending steps survive.
//
// This is a critical scenario because:
// 1. Claude can run `git rebase` via the Bash tool
// 2. No new prompt is submitted between the rebase and the next checkpoint
//
// The test validates that:
// - Turn-end steps before and after the rebase are both pending
// - The session state's BaseCommit is updated correctly
func TestManualCommit_MidSessionRebaseFollowsHead(t *testing.T) {
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
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function A"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	// Create first file change
	fileAContent := pkgFuncA
	env.WriteFile("a.go", fileAContent)

	session.CreateTranscript(
		"Create function A",
		[]FileChange{{Path: "a.go", Content: fileAContent}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Verify checkpoint 1 was recorded in session state on the original base
	if state := env.AssertTurnEndRecorded(session.ID, "a.go"); state.BaseCommit != initialFeatureHead {
		t.Fatalf("BaseCommit = %s, want %s", state.BaseCommit, initialFeatureHead)
	}

	// ========================================
	// Phase 3: Simulate Claude doing a rebase (via Bash tool)
	// ========================================
	t.Log("Phase 3: Simulating Claude performing rebase via Bash tool")

	// This simulates what happens when Claude runs: git rebase master
	// Note: We're NOT calling SimulateUserPromptSubmit here because the rebase
	// happens mid-session as part of Claude's tool execution
	testutil.RunGit(t, env.RepoDir, "rebase", "master")

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
	fileBContent := pkgFuncB
	env.WriteFile("b.go", fileBContent)

	// Reset transcript builder for new checkpoint
	session.TranscriptBuilder = NewTranscriptBuilder()
	session.CreateTranscript(
		"Create function B after rebase",
		[]FileChange{{Path: "b.go", Content: fileBContent}},
	)

	// This is the critical test: SimulateStop calls SaveStep which should
	// detect HEAD has changed and move the session's BaseCommit
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2 after rebase) failed: %v", err)
	}

	// ========================================
	// Phase 5: Verify BaseCommit followed HEAD
	// ========================================
	t.Log("Phase 5: Verifying BaseCommit followed HEAD")

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

	// Verify both checkpoints' files are still pending in session state
	env.AssertTurnEndRecorded(session.ID, "a.go", "b.go")
	if state.StepCount != 2 {
		t.Errorf("Expected 2 steps after BaseCommit sync, got %d", state.StepCount)
	} else {
		t.Logf("✓ Found %d steps after BaseCommit sync", state.StepCount)
	}

	t.Log("Mid-session rebase test completed successfully!")
}

// gitCheckout is a helper to checkout a specific ref using git CLI.
// Uses CLI instead of go-git to work around go-git v5 bug with untracked files.
func (env *TestEnv) gitCheckout(ref string) {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", "checkout", ref)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		env.T.Fatalf("git checkout %s failed: %v\nOutput: %s", ref, err, output)
	}
}

// TestManualCommit_CommitThenRebaseMidSession tests the scenario where Claude:
// 1. Creates checkpoints (pending turn-end steps)
// 2. Commits the work (triggers condensation)
// 3. Rebases onto another branch (HEAD changes)
// 4. Creates more checkpoints
//
// This verifies condensation's state reset and the BaseCommit sync in SaveStep
// compose.
func TestManualCommit_CommitThenRebaseMidSession(t *testing.T) {
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
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create function A"); err != nil {
		t.Fatalf("SimulateUserPromptSubmitWithPrompt failed: %v", err)
	}

	// Create file and checkpoint
	fileAContent := pkgFuncA
	env.WriteFile("a.go", fileAContent)

	session.CreateTranscript(
		"Create function A",
		[]FileChange{{Path: "a.go", Content: fileAContent}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// Verify checkpoint 1 was recorded
	env.AssertTurnEndRecorded(session.ID, "a.go")

	// ========================================
	// Phase 3: Claude commits (triggers condensation)
	// ========================================
	t.Log("Phase 3: Claude commits (triggers condensation)")

	// Stage and commit the file
	env.GitAdd("a.go")
	// Use GitCommitWithHooks to simulate the full commit flow with hooks
	env.GitCommitWithHooks("Add function A", "a.go")

	postCommitHead := env.GetHeadHash()
	t.Logf("After commit, feature HEAD: %s", postCommitHead[:7])

	// Verify data was condensed to metadata branch
	if !env.BranchExists(paths.MetadataBranchName) {
		t.Fatalf("%s branch should exist after condensation", paths.MetadataBranchName)
	}

	// ========================================
	// Phase 4: Claude rebases (HEAD changes again)
	// ========================================
	t.Log("Phase 4: Claude rebases onto master")

	testutil.RunGit(t, env.RepoDir, "rebase", "master")

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

	fileBContent := pkgFuncB
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
	// - Condensation reset the session's pending work
	// - HEAD changed twice (commit, then rebase)
	// - Session state still has old BaseCommit
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2 after commit+rebase) failed: %v", err)
	}

	// ========================================
	// Phase 6: Verify correct behavior
	// ========================================
	t.Log("Phase 6: Verifying correct behavior")

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

	// The new turn-end step recorded b.go
	env.AssertTurnEndRecorded(session.ID, "b.go")

	t.Log("Commit-then-rebase mid-session test completed successfully!")
}
