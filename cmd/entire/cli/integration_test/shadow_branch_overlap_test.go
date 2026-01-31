//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"

	"entire.io/cli/cmd/entire/cli/strategy"
)

// TestShadowBranchOverlap_ContinueWork tests scenario 1: continuing work on the same files.
// Session A modifies file1.ts, Session B continues working on file1.ts.
// Expected: Both sessions continue on the same shadow branch (file overlap detected).
func TestShadowBranchOverlap_ContinueWork(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	expectedShadowBranch := "entire/" + initialHead[:7]

	// Session A: Modify file1.ts
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	file1Content := "export const auth = () => { return true; }"
	env.WriteFile("src/file1.ts", file1Content)
	session1.CreateTranscript(
		"Implement auth function",
		[]FileChange{{Path: "src/file1.ts", Content: file1Content}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch exists
	if !env.BranchExists(expectedShadowBranch) {
		t.Fatalf("Expected shadow branch %s to exist after session 1", expectedShadowBranch)
	}

	// Session B: Continue working on file1.ts (same session continues)
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 2) failed: %v", err)
	}

	file1ContentV2 := "export const auth = () => { return validateToken(); }"
	env.WriteFile("src/file1.ts", file1ContentV2)
	session1.TranscriptBuilder = NewTranscriptBuilder()
	session1.CreateTranscript(
		"Add token validation",
		[]FileChange{{Path: "src/file1.ts", Content: file1ContentV2}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 2) failed: %v", err)
	}

	// Verify both checkpoints exist on same shadow branch
	points := env.GetRewindPoints()
	if len(points) != 2 {
		t.Fatalf("Expected 2 rewind points, got %d", len(points))
	}

	// Verify shadow branch still exists and wasn't reset
	if !env.BranchExists(expectedShadowBranch) {
		t.Errorf("Expected shadow branch %s to still exist after session 2", expectedShadowBranch)
	}
}

// TestShadowBranchOverlap_DismissAndStartFresh tests scenario 2: dismiss all work and start fresh.
// Session A modifies file1.ts, user runs git restore, Session B modifies file2.ts.
// Expected: Shadow branch is reset when Session B creates checkpoint.
func TestShadowBranchOverlap_DismissAndStartFresh(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	expectedShadowBranch := "entire/" + initialHead[:7]

	// Session A: Modify file1.ts
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	file1Content := "export const file1 = 'v1';"
	env.WriteFile("src/file1.ts", file1Content)
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "src/file1.ts", Content: file1Content}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch exists with file1.ts
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file1.ts") {
		t.Fatal("Expected src/file1.ts to exist in shadow branch after session 1")
	}

	// User dismisses changes: git restore .
	cmd := exec.Command("git", "restore", ".")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git restore failed: %v\nOutput: %s", err, output)
	}

	// Verify worktree is clean
	if env.FileExists("src/file1.ts") {
		t.Fatal("Expected src/file1.ts to be removed after git restore")
	}

	// Session B: Start new work on file2.ts (new prompt, same session)
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 2) failed: %v", err)
	}

	file2Content := "export const file2 = 'v1';"
	env.WriteFile("src/file2.ts", file2Content)
	session1.TranscriptBuilder = NewTranscriptBuilder()
	session1.CreateTranscript(
		"Create file2",
		[]FileChange{{Path: "src/file2.ts", Content: file2Content}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 2) failed: %v", err)
	}

	// Verify shadow branch was reset: file1.ts should NOT be in shadow branch
	if env.FileExistsInBranch(expectedShadowBranch, "src/file1.ts") {
		t.Error("Expected src/file1.ts to NOT exist in shadow branch after reset")
	}

	// Verify shadow branch has file2.ts
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file2.ts") {
		t.Error("Expected src/file2.ts to exist in shadow branch after session 2")
	}
}

// TestShadowBranchOverlap_PartialDismiss tests scenario 3: dismiss some files, keep others.
// Session A modifies file1.ts and file2.ts, user restores file1.ts, Session B modifies file2.ts and file3.ts.
// Expected: Shadow branch continues (file2.ts overlap detected).
func TestShadowBranchOverlap_PartialDismiss(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	expectedShadowBranch := "entire/" + initialHead[:7]

	// Session A: Modify file1.ts and file2.ts
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	file1Content := "export const file1 = 'v1';"
	file2Content := "export const file2 = 'v1';"
	env.WriteFile("src/file1.ts", file1Content)
	env.WriteFile("src/file2.ts", file2Content)
	session1.CreateTranscript(
		"Create file1 and file2",
		[]FileChange{
			{Path: "src/file1.ts", Content: file1Content},
			{Path: "src/file2.ts", Content: file2Content},
		},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch has both files
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file1.ts") {
		t.Fatal("Expected src/file1.ts to exist in shadow branch")
	}
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file2.ts") {
		t.Fatal("Expected src/file2.ts to exist in shadow branch")
	}

	// User dismisses file1.ts but keeps file2.ts
	cmd := exec.Command("git", "restore", "src/file1.ts")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git restore failed: %v\nOutput: %s", err, output)
	}

	// Verify file1.ts is gone but file2.ts remains
	if env.FileExists("src/file1.ts") {
		t.Fatal("Expected src/file1.ts to be removed after git restore")
	}
	if !env.FileExists("src/file2.ts") {
		t.Fatal("Expected src/file2.ts to still exist")
	}

	// Session B: Modify file2.ts and add file3.ts
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 2) failed: %v", err)
	}

	file2ContentV2 := "export const file2 = 'v2';"
	file3Content := "export const file3 = 'v1';"
	env.WriteFile("src/file2.ts", file2ContentV2)
	env.WriteFile("src/file3.ts", file3Content)
	session1.TranscriptBuilder = NewTranscriptBuilder()
	session1.CreateTranscript(
		"Update file2 and create file3",
		[]FileChange{
			{Path: "src/file2.ts", Content: file2ContentV2},
			{Path: "src/file3.ts", Content: file3Content},
		},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 2) failed: %v", err)
	}

	// Verify shadow branch was NOT reset (overlap on file2.ts)
	// Shadow branch should still have file1.ts from session 1
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file1.ts") {
		t.Error("Expected src/file1.ts to still exist in shadow branch (not reset)")
	}

	// Verify session 2 files are in shadow branch
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file2.ts") {
		t.Error("Expected src/file2.ts to exist in shadow branch")
	}
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file3.ts") {
		t.Error("Expected src/file3.ts to exist in shadow branch")
	}
}

// TestShadowBranchOverlap_StashAnswerQuestionsUnstash tests scenario 4: stash, ask questions, unstash.
// Session A modifies file1.ts, user stashes, Session B just answers questions (no checkpoint),
// user unstashes and commits.
// Expected: Session A's checkpoint data is preserved and used for condensation.
func TestShadowBranchOverlap_StashAnswerQuestionsUnstash(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	expectedShadowBranch := "entire/" + initialHead[:7]

	// Session A: Modify file1.ts
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	file1Content := "export const file1 = 'v1';"
	env.WriteFile("src/file1.ts", file1Content)
	session1.CreateTranscript(
		"Create file1",
		[]FileChange{{Path: "src/file1.ts", Content: file1Content}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch exists
	if !env.BranchExists(expectedShadowBranch) {
		t.Fatal("Expected shadow branch to exist after session 1")
	}

	// User stashes changes
	cmd := exec.Command("git", "stash")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git stash failed: %v\nOutput: %s", err, output)
	}

	// Verify worktree is clean
	if env.FileExists("src/file1.ts") {
		t.Fatal("Expected src/file1.ts to be removed after git stash")
	}

	// Session B: Just answer questions, no code changes (no checkpoint)
	// We simulate a prompt but don't write files or call Stop
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 2) failed: %v", err)
	}
	// Note: No SimulateStop call here - simulating that the session just answered questions

	// User unstashes changes
	cmd = exec.Command("git", "stash", "pop")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git stash pop failed: %v\nOutput: %s", err, output)
	}

	// Verify file is back
	if !env.FileExists("src/file1.ts") {
		t.Fatal("Expected src/file1.ts to be restored after git stash pop")
	}

	// Session C: Continue with the restored work
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 3) failed: %v", err)
	}

	// User commits
	env.GitCommitWithShadowHooks("Add authentication", "src/file1.ts")

	// Verify shadow branch still exists and was used for condensation
	// (checking that Session A's data was preserved through the stash/unstash cycle)
	points := env.GetRewindPoints()
	if len(points) == 0 {
		t.Fatal("Expected at least 1 rewind point after commit")
	}
}

// TestShadowBranchOverlap_StashNewWorkSameFiles tests scenario 5: stash, new work on same files.
// Session A modifies file1.ts, user stashes, Session B modifies file1.ts (same file!).
// Expected: Shadow branch is reset (worktree was clean at prompt start, not a continuation).
func TestShadowBranchOverlap_StashNewWorkSameFiles(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	expectedShadowBranch := "entire/" + initialHead[:7]

	// Session A: Modify file1.ts
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	file1ContentA := "export const login = () => { /* Session A */ };"
	env.WriteFile("src/file1.ts", file1ContentA)
	session1.CreateTranscript(
		"Implement login (Session A)",
		[]FileChange{{Path: "src/file1.ts", Content: file1ContentA}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch has Session A's version
	content, found := env.ReadFileFromBranch(expectedShadowBranch, "src/file1.ts")
	if !found {
		t.Fatal("Expected src/file1.ts to exist in shadow branch after session 1")
	}
	if !strings.Contains(content, "Session A") {
		t.Errorf("Expected shadow branch to contain Session A's code, got: %s", content)
	}

	// User stashes changes
	cmd := exec.Command("git", "stash")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git stash failed: %v\nOutput: %s", err, output)
	}

	// Session B: Modify file1.ts (same file, but should reset because worktree was clean at prompt start)
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 2) failed: %v", err)
	}

	file1ContentB := "export const login = () => { /* Session B */ };"
	env.WriteFile("src/file1.ts", file1ContentB)
	session1.TranscriptBuilder = NewTranscriptBuilder()
	session1.CreateTranscript(
		"Implement login (Session B)",
		[]FileChange{{Path: "src/file1.ts", Content: file1ContentB}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 2) failed: %v", err)
	}

	// Verify shadow branch was reset: should have Session B's version, not Session A's
	content, found = env.ReadFileFromBranch(expectedShadowBranch, "src/file1.ts")
	if !found {
		t.Fatal("Expected src/file1.ts to exist in shadow branch after session 2")
	}
	if strings.Contains(content, "Session A") {
		t.Errorf("Expected shadow branch to be reset and NOT contain Session A's code, got: %s", content)
	}
	if !strings.Contains(content, "Session B") {
		t.Errorf("Expected shadow branch to contain Session B's code, got: %s", content)
	}
}

// TestShadowBranchOverlap_StashNewWorkDifferentFiles tests scenario 6: stash, new work on different files.
// Session A modifies file1.ts, user stashes, Session B modifies file2.ts (different file).
// Expected: Shadow branch is reset, Session A's data is lost (accepted limitation).
func TestShadowBranchOverlap_StashNewWorkDifferentFiles(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t, strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	expectedShadowBranch := "entire/" + initialHead[:7]

	// Session A: Modify file1.ts
	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 1) failed: %v", err)
	}

	file1Content := "export const file1 = 'session A';"
	env.WriteFile("src/file1.ts", file1Content)
	session1.CreateTranscript(
		"Create file1 (Session A)",
		[]FileChange{{Path: "src/file1.ts", Content: file1Content}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 1) failed: %v", err)
	}

	// Verify shadow branch has file1.ts
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file1.ts") {
		t.Fatal("Expected src/file1.ts to exist in shadow branch after session 1")
	}

	// User stashes changes
	cmd := exec.Command("git", "stash")
	cmd.Dir = env.RepoDir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git stash failed: %v\nOutput: %s", err, output)
	}

	// Session B: Modify file2.ts (different file, no overlap)
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (session 2) failed: %v", err)
	}

	file2Content := "export const file2 = 'session B';"
	env.WriteFile("src/file2.ts", file2Content)
	session1.TranscriptBuilder = NewTranscriptBuilder()
	session1.CreateTranscript(
		"Create file2 (Session B)",
		[]FileChange{{Path: "src/file2.ts", Content: file2Content}},
	)
	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (session 2) failed: %v", err)
	}

	// Verify shadow branch was reset: file1.ts should NOT be in shadow branch
	if env.FileExistsInBranch(expectedShadowBranch, "src/file1.ts") {
		t.Error("Expected src/file1.ts to NOT exist in shadow branch after reset (Session A's data lost)")
	}

	// Verify shadow branch has file2.ts
	if !env.FileExistsInBranch(expectedShadowBranch, "src/file2.ts") {
		t.Error("Expected src/file2.ts to exist in shadow branch after session 2")
	}

	// This is the accepted limitation: if user unstashes file1.ts and commits,
	// only Session B's transcript will be available for condensation
}
