//go:build integration

package integration

import (
	"encoding/json"
	"testing"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
	"entire.io/cli/cmd/entire/cli/paths"
	"entire.io/cli/cmd/entire/cli/strategy"
	"entire.io/cli/cmd/entire/cli/trailers"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestManualCommit_Attribution tests the full attribution calculation flow:
// 1. Agent creates checkpoint 1
// 2. User makes changes between checkpoints
// 3. User enters new prompt (attribution calculated at prompt start)
// 4. Agent creates checkpoint 2
// 5. User commits (condensation happens with attribution)
// 6. Verify attribution metadata is correct
func TestManualCommit_Attribution(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	// Create initial commit
	env.WriteFile("main.go", "package main\n")
	env.GitAdd("main.go")
	env.GitCommit("Initial commit")

	env.InitEntire(strategy.StrategyNameManualCommit)

	initialHead := env.GetHeadHash()
	t.Logf("Initial HEAD: %s", initialHead[:7])

	// ========================================
	// CHECKPOINT 1: Agent adds function
	// ========================================
	t.Log("Creating checkpoint 1 (agent adds function)")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (prompt 1) failed: %v", err)
	}

	// Agent adds 4 lines
	checkpoint1Content := "package main\n\nfunc agentFunc() {\n\treturn 42\n}\n"
	env.WriteFile("main.go", checkpoint1Content)

	session.CreateTranscript(
		"Add agent function",
		[]FileChange{{Path: "main.go", Content: checkpoint1Content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 1) failed: %v", err)
	}

	// ========================================
	// USER EDITS between checkpoints
	// ========================================
	t.Log("User makes edits between checkpoints")

	// User adds 5 comment lines
	userContent := checkpoint1Content +
		"// User comment 1\n" +
		"// User comment 2\n" +
		"// User comment 3\n" +
		"// User comment 4\n" +
		"// User comment 5\n"
	env.WriteFile("main.go", userContent)

	// ========================================
	// CHECKPOINT 2: New prompt (attribution calculated)
	// ========================================
	t.Log("User enters new prompt (attribution should capture 5 user lines)")

	// Simulate UserPromptSubmit hook - this calculates attribution at prompt start
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (prompt 2) failed: %v", err)
	}

	// Agent adds another function (4 more lines)
	checkpoint2Content := userContent + "\nfunc agentFunc2() {\n\treturn 100\n}\n"
	env.WriteFile("main.go", checkpoint2Content)

	session.CreateTranscript(
		"Add second agent function",
		[]FileChange{{Path: "main.go", Content: checkpoint2Content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (checkpoint 2) failed: %v", err)
	}

	// Verify 2 rewind points
	points := env.GetRewindPoints()
	if len(points) != 2 {
		t.Fatalf("Expected 2 rewind points, got %d", len(points))
	}

	// ========================================
	// USER COMMITS: Condensation happens
	// ========================================
	t.Log("User commits (condensation should happen)")

	// Commit using hooks (this triggers condensation)
	env.GitCommitWithShadowHooks("Add functions", "main.go")

	// Get commit hash and checkpoint ID
	headHash := env.GetHeadHash()
	t.Logf("User commit: %s", headHash[:7])

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	commitObj, err := repo.CommitObject(plumbing.NewHash(headHash))
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	checkpointID, found := trailers.ParseCheckpoint(commitObj.Message)
	if !found {
		t.Fatal("Commit should have Entire-Checkpoint trailer")
	}
	t.Logf("Checkpoint ID: %s", checkpointID)

	// ========================================
	// VERIFY ATTRIBUTION
	// ========================================
	t.Log("Verifying attribution in metadata")

	// Read metadata from entire/sessions branch
	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("Failed to get entire/sessions branch: %v", err)
	}

	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("Failed to get sessions commit: %v", err)
	}

	sessionsTree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("Failed to get sessions tree: %v", err)
	}

	// Read session-level metadata.json from sharded path (InitialAttribution is in 0/metadata.json)
	metadataPath := SessionMetadataPath(checkpointID.String())
	metadataFile, err := sessionsTree.File(metadataPath)
	if err != nil {
		t.Fatalf("Failed to read session metadata.json at path %s: %v", metadataPath, err)
	}

	metadataContent, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("Failed to read metadata content: %v", err)
	}

	var metadata checkpoint.CommittedMetadata
	if err := json.Unmarshal([]byte(metadataContent), &metadata); err != nil {
		t.Fatalf("Failed to parse metadata.json: %v", err)
	}

	// Verify InitialAttribution exists
	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution is nil")
	}

	attr := metadata.InitialAttribution
	t.Logf("Attribution: agent=%d, human_added=%d, human_modified=%d, human_removed=%d, total=%d, percentage=%.1f%%",
		attr.AgentLines, attr.HumanAdded, attr.HumanModified, attr.HumanRemoved,
		attr.TotalCommitted, attr.AgentPercentage)

	// Verify attribution was calculated and has reasonable values
	// Note: The shadow branch includes all worktree changes (agent + user),
	// so base→shadow diff includes user edits that were present during SaveChanges.
	// The attribution separates them using PromptAttributions.
	//
	// Expected: agent=13 (base→shadow includes user comments in worktree)
	//           human=5 (from PromptAttribution)
	//           total=18 (net additions)
	//
	// This tests that:
	// 1. Attribution is calculated and stored
	// 2. PromptAttribution captured user edits between checkpoints
	// 3. Percentages are computed
	if attr.AgentLines <= 0 {
		t.Errorf("AgentLines = %d, should be > 0", attr.AgentLines)
	}

	if attr.HumanAdded != 5 {
		t.Errorf("HumanAdded = %d, want 5 (5 comments captured in PromptAttribution)",
			attr.HumanAdded)
	}

	if attr.TotalCommitted <= 0 {
		t.Errorf("TotalCommitted = %d, should be > 0", attr.TotalCommitted)
	}

	if attr.AgentPercentage <= 0 || attr.AgentPercentage >= 100 {
		t.Errorf("AgentPercentage = %.1f%%, should be between 0 and 100",
			attr.AgentPercentage)
	}
}

// TestManualCommit_AttributionDeletionOnly tests attribution for deletion-only commits
func TestManualCommit_AttributionDeletionOnly(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	// Create initial commit with content
	initialContent := "package main\n\nfunc oldFunc1() {}\nfunc oldFunc2() {}\nfunc oldFunc3() {}\n"
	env.WriteFile("main.go", initialContent)
	env.GitAdd("main.go")
	env.GitCommit("Initial commit")

	env.InitEntire(strategy.StrategyNameManualCommit)

	// ========================================
	// CHECKPOINT 1: Agent REMOVES a function (deletion, no additions)
	// ========================================
	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Agent removes one function (keeps 2 functions)
	checkpointContent := "package main\n\nfunc oldFunc2() {}\nfunc oldFunc3() {}\n"
	env.WriteFile("main.go", checkpointContent)

	session.CreateTranscript(
		"Remove oldFunc1",
		[]FileChange{{Path: "main.go", Content: checkpointContent}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	// ========================================
	// USER DELETES REMAINING FUNCTIONS
	// ========================================
	t.Log("User deletes remaining functions (deletion-only commit)")

	// Remove remaining functions, keep only package declaration
	env.WriteFile("main.go", "package main\n")

	// Commit using hooks
	env.GitCommitWithShadowHooks("Remove remaining functions", "main.go")

	// Get checkpoint ID
	headHash := env.GetHeadHash()
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	commitObj, err := repo.CommitObject(plumbing.NewHash(headHash))
	if err != nil {
		t.Fatalf("failed to get commit object: %v", err)
	}

	checkpointID, found := trailers.ParseCheckpoint(commitObj.Message)
	if !found {
		t.Fatal("Commit should have Entire-Checkpoint trailer")
	}

	// ========================================
	// VERIFY ATTRIBUTION FOR DELETION-ONLY COMMIT
	// ========================================
	t.Log("Verifying attribution for deletion-only commit")

	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("Failed to get entire/sessions branch: %v", err)
	}

	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("Failed to get sessions commit: %v", err)
	}

	sessionsTree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("Failed to get sessions tree: %v", err)
	}

	// Read session-level metadata.json (InitialAttribution is in 0/metadata.json)
	metadataPath := SessionMetadataPath(checkpointID.String())
	metadataFile, err := sessionsTree.File(metadataPath)
	if err != nil {
		t.Fatalf("Failed to read session metadata.json at path %s: %v", metadataPath, err)
	}

	metadataContent, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("Failed to read metadata content: %v", err)
	}

	var metadata checkpoint.CommittedMetadata
	if err := json.Unmarshal([]byte(metadataContent), &metadata); err != nil {
		t.Fatalf("Failed to parse metadata.json: %v", err)
	}

	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution is nil")
	}

	attr := metadata.InitialAttribution
	t.Logf("Attribution (deletion-only): agent=%d, human_added=%d, human_removed=%d, total=%d, percentage=%.1f%%",
		attr.AgentLines, attr.HumanAdded, attr.HumanRemoved,
		attr.TotalCommitted, attr.AgentPercentage)

	// For deletion-only commits where agent makes no additions:
	// - Agent removed oldFunc1 (made deletions, not additions)
	// - AgentLines = 0 (no additions)
	// - User removed oldFunc2 and oldFunc3
	// - HumanAdded = 0 (no new lines)
	// - HumanRemoved = number of lines user deleted
	// - TotalCommitted = 0 (no additions from anyone)
	// - AgentPercentage = 0 (by convention for deletion-only)

	if attr.AgentLines != 0 {
		t.Errorf("AgentLines = %d, want 0 (agent made no additions, only deletions)", attr.AgentLines)
	}

	if attr.HumanAdded != 0 {
		t.Errorf("HumanAdded = %d, want 0 (no new lines in deletion-only commit)", attr.HumanAdded)
	}

	// User removed 2 remaining functions + 1 blank line (3 lines total)
	if attr.HumanRemoved != 3 {
		t.Errorf("HumanRemoved = %d, want 3 (removed blank + 2 functions = 3 lines)", attr.HumanRemoved)
	}

	if attr.TotalCommitted != 0 {
		t.Errorf("TotalCommitted = %d, want 0 (deletion-only commit has no net additions)", attr.TotalCommitted)
	}

	if attr.AgentPercentage != 0 {
		t.Errorf("AgentPercentage = %.1f%%, want 0 (deletion-only commit)",
			attr.AgentPercentage)
	}
}

// TestManualCommit_AttributionNoDoubleCount tests that PromptAttributions are
// cleared after condensation to prevent double-counting on subsequent commits.
//
// Bug scenario:
// 1. Checkpoint 1 → user edits → commit (condensation, PromptAttributions used)
// 2. CheckpointCount reset to 0, but PromptAttributions NOT cleared
// 3. Checkpoint 2 → new PromptAttributions appended to old ones
// 4. Second commit → CalculateAttributionWithAccumulated sums ALL PromptAttributions
// 5. User edits from first commit are double-counted
func TestManualCommit_AttributionNoDoubleCount(t *testing.T) {
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	// Create initial commit
	env.WriteFile("main.go", "package main\n")
	env.GitAdd("main.go")
	env.GitCommit("Initial commit")

	env.InitEntire(strategy.StrategyNameManualCommit)

	// ========================================
	// FIRST CYCLE: Checkpoint → user edit → commit
	// ========================================
	t.Log("First cycle: agent checkpoint + user edit + commit")

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (first cycle) failed: %v", err)
	}

	// Agent adds 5 lines
	checkpoint1Content := "package main\n\nfunc agent1() { return 1 }\nfunc agent2() { return 2 }\nfunc agent3() { return 3 }\n"
	env.WriteFile("main.go", checkpoint1Content)

	session.CreateTranscript(
		"Add agent functions",
		[]FileChange{{Path: "main.go", Content: checkpoint1Content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (first cycle) failed: %v", err)
	}

	// User adds 2 lines between checkpoints
	userEdit1Content := checkpoint1Content + "// User comment 1\n// User comment 2\n"
	env.WriteFile("main.go", userEdit1Content)

	// Commit with hooks (condensation happens)
	env.GitCommitWithShadowHooks("First commit", "main.go")

	// Get first commit's checkpoint ID
	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	commit1, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get commit: %v", err)
	}

	checkpointID1, found := trailers.ParseCheckpoint(commit1.Message)
	if !found {
		t.Fatal("First commit should have checkpoint trailer")
	}

	t.Logf("First commit checkpoint ID: %s", checkpointID1)

	// Verify first commit attribution
	attr1 := getAttributionFromMetadata(t, repo, checkpointID1)
	t.Logf("First commit attribution: agent=%d, human_added=%d, total=%d",
		attr1.AgentLines, attr1.HumanAdded, attr1.TotalCommitted)

	// First commit should have:
	// - Agent: 4 lines (3 functions + 1 blank)
	// - User: 2 lines (2 comments)
	// - Total: 6 lines
	if attr1.HumanAdded != 2 {
		t.Errorf("First commit HumanAdded = %d, want 2", attr1.HumanAdded)
	}

	// ========================================
	// SECOND CYCLE: New checkpoint → user edit → commit
	// ========================================
	t.Log("Second cycle: new agent checkpoint + user edit + commit")

	// Simulate new prompt (should calculate attribution, which should be empty after reset)
	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit (second cycle) failed: %v", err)
	}

	// Agent adds 3 more lines
	checkpoint2Content := userEdit1Content + "\nfunc agent4() { return 4 }\nfunc agent5() { return 5 }\n"
	env.WriteFile("main.go", checkpoint2Content)

	session.CreateTranscript(
		"Add more agent functions",
		[]FileChange{{Path: "main.go", Content: checkpoint2Content}},
	)
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (second cycle) failed: %v", err)
	}

	// User adds 1 more line
	userEdit2Content := checkpoint2Content + "// User comment 3\n"
	env.WriteFile("main.go", userEdit2Content)

	// Second commit (another condensation)
	env.GitCommitWithShadowHooks("Second commit", "main.go")

	// Get second commit's checkpoint ID
	head, err = repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD after second commit: %v", err)
	}

	commit2, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("failed to get second commit: %v", err)
	}

	checkpointID2, found := trailers.ParseCheckpoint(commit2.Message)
	if !found {
		t.Fatal("Second commit should have checkpoint trailer")
	}

	t.Logf("Second commit checkpoint ID: %s", checkpointID2)

	// Verify second commit attribution
	attr2 := getAttributionFromMetadata(t, repo, checkpointID2)
	t.Logf("Second commit attribution: agent=%d, human_added=%d, total=%d",
		attr2.AgentLines, attr2.HumanAdded, attr2.TotalCommitted)

	// Second commit should have (since first commit):
	// - Agent: 3 lines (2 functions + 1 blank)
	// - User: 1 line (1 comment)
	// - Total: 4 lines
	//
	// BUG (if not fixed): HumanAdded would be 3 (1 new + 2 from first commit double-counted)
	// CORRECT (after fix): HumanAdded should be 1 (only new user edits)

	if attr2.HumanAdded != 1 {
		t.Errorf("Second commit HumanAdded = %d, want 1 (should NOT double-count first commit's 2 user lines)",
			attr2.HumanAdded)
	}

	if attr2.TotalCommitted != 4 {
		t.Errorf("Second commit TotalCommitted = %d, want 4 (3 agent + 1 user)",
			attr2.TotalCommitted)
	}

	// Agent percentage should be 3/4 = 75%
	if attr2.AgentPercentage < 74.9 || attr2.AgentPercentage > 75.1 {
		t.Errorf("Second commit AgentPercentage = %.1f%%, want 75.0%%", attr2.AgentPercentage)
	}
}

// getAttributionFromMetadata reads attribution from a checkpoint on entire/sessions branch.
// InitialAttribution is stored in session-level metadata (0/metadata.json).
func getAttributionFromMetadata(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID) *checkpoint.InitialAttribution {
	t.Helper()

	sessionsRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	if err != nil {
		t.Fatalf("Failed to get entire/sessions branch: %v", err)
	}

	sessionsCommit, err := repo.CommitObject(sessionsRef.Hash())
	if err != nil {
		t.Fatalf("Failed to get sessions commit: %v", err)
	}

	sessionsTree, err := sessionsCommit.Tree()
	if err != nil {
		t.Fatalf("Failed to get sessions tree: %v", err)
	}

	// Read session-level metadata (InitialAttribution is in 0/metadata.json)
	metadataPath := SessionMetadataPath(checkpointID.String())
	metadataFile, err := sessionsTree.File(metadataPath)
	if err != nil {
		t.Fatalf("Failed to read session metadata.json at path %s: %v", metadataPath, err)
	}

	metadataContent, err := metadataFile.Contents()
	if err != nil {
		t.Fatalf("Failed to read metadata content: %v", err)
	}

	var metadata checkpoint.CommittedMetadata
	if err := json.Unmarshal([]byte(metadataContent), &metadata); err != nil {
		t.Fatalf("Failed to parse metadata.json: %v", err)
	}

	if metadata.InitialAttribution == nil {
		t.Fatal("InitialAttribution is nil")
	}

	return metadata.InitialAttribution
}
