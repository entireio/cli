//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// TestLogsOnlyRewind_AppearsInRewindList verifies that after user commits with
// shadow hooks, the commits appear in the rewind list as logs-only points.
//
// Workflow:
// 1. Session creates checkpoint
// 2. User commits (triggers condensation, shadow branch pruned)
// 3. Verify commit appears in rewind list with IsLogsOnly=true
func TestLogsOnlyRewind_AppearsInRewindList(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/logs-only-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create session and checkpoint")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file
	content := "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	env.WriteFile("main.go", content)

	session.CreateTranscript(
		"Create main.go with hello world",
		[]FileChange{{Path: "main.go", Content: content}},
	)

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify checkpoint exists (not logs-only yet)
	points := env.GetRewindPoints()
	if len(points) != 1 {
		t.Fatalf("Expected 1 rewind point before commit, got %d", len(points))
	}
	if points[0].IsLogsOnly {
		t.Error("Before user commit, point should NOT be logs-only (has active checkpoint)")
	}

	t.Log("Phase 2: User commits (triggers condensation)")

	env.GitCommitWithShadowHooks("Add main.go", "main.go")

	// Get commit hash and condensation ID (now from entire/checkpoints/v1 branch, not commit trailer)
	commitHash := env.GetHeadHash()
	condensationID := env.GetLatestCondensationID()
	t.Logf("Condensation ID: %s", condensationID)

	t.Log("Phase 3: Verify logs-only point appears in rewind list")

	// Get rewind points again
	points = env.GetRewindPoints()

	// Find the logs-only point
	var logsOnlyPoint *RewindPoint
	for i := range points {
		if points[i].IsLogsOnly {
			logsOnlyPoint = &points[i]
			break
		}
	}

	if logsOnlyPoint == nil {
		t.Fatal("Expected to find a logs-only rewind point after user commit")
	}

	// Verify logs-only point properties
	if logsOnlyPoint.ID != commitHash {
		t.Errorf("Logs-only point ID should be commit hash %s, got %s", commitHash, logsOnlyPoint.ID)
	}

	if logsOnlyPoint.CondensationID != condensationID {
		t.Errorf("Logs-only point should have CondensationID %s, got %s", condensationID, logsOnlyPoint.CondensationID)
	}

	if !strings.Contains(logsOnlyPoint.Message, "Add main.go") {
		t.Errorf("Logs-only point message should contain commit message, got: %s", logsOnlyPoint.Message)
	}

	t.Logf("Found logs-only point: ID=%s, Message=%s, CondensationID=%s",
		logsOnlyPoint.ID[:7], logsOnlyPoint.Message, logsOnlyPoint.CondensationID)
}

// TestLogsOnlyRewind_RestoresTranscript verifies that restoring from a logs-only
// point copies the session transcript to Claude's project directory.
func TestLogsOnlyRewind_RestoresTranscript(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/logs-restore-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create session and commit")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "package main\n\nfunc test() {}\n"
	env.WriteFile("test.go", content)

	// Add recognizable content to transcript
	session.TranscriptBuilder.AddUserMessage("Create a test function")
	session.TranscriptBuilder.AddAssistantMessage("I'll create a test function for you.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "test.go", content)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done! Created test.go with a test function.")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// User commits
	env.GitCommitWithShadowHooks("Add test function", "test.go")
	commitHash := env.GetHeadHash()

	t.Log("Phase 2: Clear Claude project directory")

	// Remove any existing transcripts
	entries, err := os.ReadDir(env.ClaudeProjectDir)
	if err == nil {
		for _, entry := range entries {
			os.Remove(filepath.Join(env.ClaudeProjectDir, entry.Name()))
		}
	}

	t.Log("Phase 3: Restore logs from logs-only point")

	// Get the logs-only point
	points := env.GetRewindPoints()
	var logsOnlyPoint *RewindPoint
	for i := range points {
		if points[i].IsLogsOnly && points[i].ID == commitHash {
			logsOnlyPoint = &points[i]
			break
		}
	}

	if logsOnlyPoint == nil {
		t.Fatal("Could not find logs-only point for commit")
	}

	// Perform logs-only rewind
	if err := env.RewindLogsOnly(logsOnlyPoint.ID); err != nil {
		t.Fatalf("RewindLogsOnly failed: %v", err)
	}

	t.Log("Phase 4: Verify transcript restored")

	// Find restored transcript file
	entries, err = os.ReadDir(env.ClaudeProjectDir)
	if err != nil {
		t.Fatalf("Failed to read Claude project dir: %v", err)
	}

	var transcriptFile string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			transcriptFile = filepath.Join(env.ClaudeProjectDir, entry.Name())
			break
		}
	}

	if transcriptFile == "" {
		t.Fatal("No transcript file found in Claude project directory after logs-only rewind")
	}

	// Read and verify transcript content
	transcriptContent := env.ReadFileAbsolute(transcriptFile)
	if !strings.Contains(transcriptContent, "Create a test function") {
		t.Error("Restored transcript should contain the original user prompt")
	}
	if !strings.Contains(transcriptContent, "test.go") {
		t.Error("Restored transcript should contain the file path")
	}

	t.Logf("Transcript restored to: %s", transcriptFile)
}

// TestLogsOnlyRewind_DoesNotModifyWorkingDirectory verifies that logs-only
// rewind does NOT modify the working directory files.
func TestLogsOnlyRewind_DoesNotModifyWorkingDirectory(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/no-modify-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create session 1 and commit")

	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	v1Content := "version 1"
	env.WriteFile("file.txt", v1Content)
	session1.CreateTranscript("Create file with version 1", []FileChange{{Path: "file.txt", Content: v1Content}})

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add file version 1", "file.txt")
	commit1Hash := env.GetHeadHash()
	t.Logf("Commit 1: %s", commit1Hash[:7])

	// Clear session 1 state to simulate session completion (avoids concurrent session warning)
	if err := env.ClearSessionState(session1.ID); err != nil {
		t.Fatalf("ClearSessionState failed: %v", err)
	}

	t.Log("Phase 2: Create session 2 and commit")

	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	v2Content := "version 2"
	env.WriteFile("file.txt", v2Content)
	session2.CreateTranscript("Update file to version 2", []FileChange{{Path: "file.txt", Content: v2Content}})

	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Update file to version 2", "file.txt")
	t.Logf("Commit 2: %s", env.GetHeadHash()[:7])

	// Verify current state is v2
	if content := env.ReadFile("file.txt"); content != v2Content {
		t.Errorf("File should be at version 2, got: %s", content)
	}

	t.Log("Phase 3: Logs-only rewind to commit 1")

	// Get logs-only point for commit 1
	points := env.GetRewindPoints()
	var commit1Point *RewindPoint
	for i := range points {
		if points[i].IsLogsOnly && points[i].ID == commit1Hash {
			commit1Point = &points[i]
			break
		}
	}

	if commit1Point == nil {
		t.Fatal("Could not find logs-only point for commit 1")
	}

	// Perform logs-only rewind
	if err := env.RewindLogsOnly(commit1Point.ID); err != nil {
		t.Fatalf("RewindLogsOnly failed: %v", err)
	}

	t.Log("Phase 4: Verify working directory NOT modified")

	// CRITICAL: File should still be at version 2 (logs-only doesn't touch files)
	if content := env.ReadFile("file.txt"); content != v2Content {
		t.Errorf("After logs-only rewind, file should STILL be at version 2, but got: %s", content)
	}

	// HEAD should still be at commit 2
	currentHead := env.GetHeadHash()
	if currentHead == commit1Hash {
		t.Error("HEAD should NOT change after logs-only rewind")
	}

	// Branch should still be feature/no-modify-test (not detached)
	currentBranch := env.GetCurrentBranch()
	if currentBranch != "feature/no-modify-test" {
		t.Errorf("Should still be on feature branch, got: %s", currentBranch)
	}
}

// TestLogsOnlyRewind_DeduplicationWithCheckpoints verifies that a commit
// that still has an active checkpoint is NOT shown as logs-only.
func TestLogsOnlyRewind_DeduplicationWithCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/dedup-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create checkpoint (but don't commit yet)")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "package main\n\nfunc dedup() {}\n"
	env.WriteFile("dedup.go", content)
	session.CreateTranscript("Create dedup function", []FileChange{{Path: "dedup.go", Content: content}})

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// Verify we have 1 checkpoint point (not logs-only)
	points := env.GetRewindPoints()
	if len(points) != 1 {
		t.Fatalf("Expected 1 rewind point, got %d", len(points))
	}
	if points[0].IsLogsOnly {
		t.Error("Before commit, point should be checkpoint (not logs-only)")
	}
	t.Logf("Checkpoint point: %s", points[0].ID[:7])

	t.Log("Phase 2: User commits - checkpoint condensed, becomes logs-only")

	env.GitCommitWithShadowHooks("Add dedup function", "dedup.go")
	commitHash := env.GetHeadHash()

	// After commit, the checkpoint is condensed
	// We should see either:
	// 1. A logs-only point (if shadow branch was pruned)
	// 2. A checkpoint point (if shadow branch still exists)
	// But NOT both for the same commit

	points = env.GetRewindPoints()

	// Count how many points reference this commit hash
	matchingPoints := 0
	var foundLogsOnly, foundCheckpoint bool
	for _, p := range points {
		if p.ID == commitHash {
			matchingPoints++
			if p.IsLogsOnly {
				foundLogsOnly = true
			} else {
				foundCheckpoint = true
			}
		}
	}

	// Should not have duplicates
	if matchingPoints > 1 {
		t.Errorf("Commit %s should not appear multiple times in rewind list (found %d)", commitHash[:7], matchingPoints)
	}

	// Should have exactly one representation
	if foundLogsOnly && foundCheckpoint {
		t.Error("Same commit should not appear as both checkpoint AND logs-only")
	}

	t.Logf("Found points: logsOnly=%v, checkpoint=%v, total=%d", foundLogsOnly, foundCheckpoint, len(points))
}

// TestLogsOnlyRewind_MultipleCommits verifies logs-only points from
// multiple commits in history.
func TestLogsOnlyRewind_MultipleCommits(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/multi-commit-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	var commitHashes []string

	// Create 3 sessions, each followed by a commit
	for i := 1; i <= 3; i++ {
		t.Logf("Creating session %d and committing", i)

		session := env.NewSession()
		if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
			t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
		}

		filename := "file" + string(rune('0'+i)) + ".go"
		content := "package main\n\n// File " + string(rune('0'+i)) + "\n"
		env.WriteFile(filename, content)
		session.CreateTranscript("Create "+filename, []FileChange{{Path: filename, Content: content}})

		if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
			t.Fatalf("SimulateStop failed: %v", err)
		}

		env.GitCommitWithShadowHooks("Add "+filename, filename)
		commitHashes = append(commitHashes, env.GetHeadHash())

		// Clear session state to simulate session completion (avoids concurrent session warning)
		if err := env.ClearSessionState(session.ID); err != nil {
			t.Fatalf("ClearSessionState failed: %v", err)
		}
	}

	t.Log("Verify all 3 commits appear as logs-only points")

	points := env.GetRewindPoints()

	// Check each commit hash appears
	for _, hash := range commitHashes {
		found := false
		for _, p := range points {
			if p.ID == hash && p.IsLogsOnly {
				found = true
				t.Logf("Found logs-only point for commit %s: %s", hash[:7], p.Message)
				break
			}
		}
		if !found {
			t.Errorf("Commit %s should appear as logs-only point", hash[:7])
		}
	}
}

// filterLogsOnlyPoints returns only logs-only points from the rewind points.
func filterLogsOnlyPoints(points []RewindPoint) []RewindPoint {
	var logsOnly []RewindPoint
	for _, p := range points {
		if p.IsLogsOnly {
			logsOnly = append(logsOnly, p)
		}
	}
	return logsOnly
}

// TestLogsOnlyRewind_Reset verifies that --reset flag performs git reset --hard.
func TestLogsOnlyRewind_Reset(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/reset-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create session 1 and commit")

	session1 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session1.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	v1Content := "version 1 content"
	env.WriteFile("file.txt", v1Content)
	session1.CreateTranscript("Create file with version 1", []FileChange{{Path: "file.txt", Content: v1Content}})

	if err := env.SimulateStop(session1.ID, session1.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add file version 1", "file.txt")
	commit1Hash := env.GetHeadHash()
	t.Logf("Commit 1: %s", commit1Hash[:7])

	// Clear session 1 state to simulate session completion (avoids concurrent session warning)
	if err := env.ClearSessionState(session1.ID); err != nil {
		t.Fatalf("ClearSessionState failed: %v", err)
	}

	t.Log("Phase 2: Create session 2 and commit")

	session2 := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session2.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	v2Content := "version 2 content"
	env.WriteFile("file.txt", v2Content)
	session2.CreateTranscript("Update file to version 2", []FileChange{{Path: "file.txt", Content: v2Content}})

	if err := env.SimulateStop(session2.ID, session2.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Update file to version 2", "file.txt")
	commit2Hash := env.GetHeadHash()
	t.Logf("Commit 2: %s", commit2Hash[:7])

	// Verify current state
	if content := env.ReadFile("file.txt"); content != v2Content {
		t.Errorf("File should be at version 2, got: %s", content)
	}
	if env.GetHeadHash() != commit2Hash {
		t.Error("HEAD should be at commit 2")
	}

	t.Log("Phase 3: Reset to commit 1")

	// Get logs-only point for commit 1
	points := env.GetRewindPoints()
	var commit1Point *RewindPoint
	for i := range points {
		if points[i].IsLogsOnly && points[i].ID == commit1Hash {
			commit1Point = &points[i]
			break
		}
	}

	if commit1Point == nil {
		t.Fatal("Could not find logs-only point for commit 1")
	}

	// Perform reset
	if err := env.RewindReset(commit1Point.ID); err != nil {
		t.Fatalf("RewindReset failed: %v", err)
	}

	t.Log("Phase 4: Verify reset was performed")

	// File should now be at version 1
	if content := env.ReadFile("file.txt"); content != v1Content {
		t.Errorf("After reset, file should be at version 1, got: %s", content)
	}

	// HEAD should now be at commit 1
	if env.GetHeadHash() != commit1Hash {
		t.Errorf("HEAD should be at commit 1 (%s), got %s", commit1Hash[:7], env.GetHeadHash()[:7])
	}

	// Should still be on the branch (not detached)
	currentBranch := env.GetCurrentBranch()
	if currentBranch != "feature/reset-test" {
		t.Errorf("Should still be on feature branch, got: %s", currentBranch)
	}
}

// TestLogsOnlyRewind_ResetRestoresTranscript verifies that reset also restores transcript.
func TestLogsOnlyRewind_ResetRestoresTranscript(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	// Setup
	env.InitRepo()
	env.WriteFile("README.md", "# Test Repository")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/reset-transcript-test")
	env.InitEntire(strategy.StrategyNameManualCommit)

	t.Log("Phase 1: Create session and commit")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	content := "test content for reset"
	env.WriteFile("reset-test.go", content)

	// Add recognizable content to transcript
	session.TranscriptBuilder.AddUserMessage("Create a file for reset testing")
	session.TranscriptBuilder.AddAssistantMessage("Creating the reset test file.")
	toolID := session.TranscriptBuilder.AddToolUse("mcp__acp__Write", "reset-test.go", content)
	session.TranscriptBuilder.AddToolResult(toolID)
	session.TranscriptBuilder.AddAssistantMessage("Done creating reset-test.go")

	if err := session.TranscriptBuilder.WriteToFile(session.TranscriptPath); err != nil {
		t.Fatalf("Failed to write transcript: %v", err)
	}

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	env.GitCommitWithShadowHooks("Add reset test file", "reset-test.go")
	commitHash := env.GetHeadHash()

	// Add another commit to have something to reset from
	env.WriteFile("another.txt", "another file")
	env.GitAdd("another.txt")
	env.GitCommit("Add another file")

	t.Log("Phase 2: Clear Claude project directory")

	entries, err := os.ReadDir(env.ClaudeProjectDir)
	if err == nil {
		for _, entry := range entries {
			os.Remove(filepath.Join(env.ClaudeProjectDir, entry.Name()))
		}
	}

	t.Log("Phase 3: Reset to first commit")

	points := env.GetRewindPoints()
	var targetPoint *RewindPoint
	for i := range points {
		if points[i].IsLogsOnly && points[i].ID == commitHash {
			targetPoint = &points[i]
			break
		}
	}

	if targetPoint == nil {
		t.Fatal("Could not find logs-only point for target commit")
	}

	if err := env.RewindReset(targetPoint.ID); err != nil {
		t.Fatalf("RewindReset failed: %v", err)
	}

	t.Log("Phase 4: Verify transcript was restored")

	entries, err = os.ReadDir(env.ClaudeProjectDir)
	if err != nil {
		t.Fatalf("Failed to read Claude project dir: %v", err)
	}

	var transcriptFile string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			transcriptFile = filepath.Join(env.ClaudeProjectDir, entry.Name())
			break
		}
	}

	if transcriptFile == "" {
		t.Fatal("No transcript file found after reset")
	}

	transcriptContent := env.ReadFileAbsolute(transcriptFile)
	if !strings.Contains(transcriptContent, "Create a file for reset testing") {
		t.Error("Restored transcript should contain the original user prompt")
	}

	// Also verify file state was reset
	if content := env.ReadFile("reset-test.go"); content != "test content for reset" {
		t.Errorf("File content should match original, got: %s", content)
	}

	// another.txt should NOT exist after reset
	if env.FileExists("another.txt") {
		t.Error("another.txt should not exist after reset to earlier commit")
	}
}
