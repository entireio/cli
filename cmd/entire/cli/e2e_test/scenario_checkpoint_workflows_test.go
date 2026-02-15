//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the scenarios documented in docs/architecture/checkpoint-scenarios.md

// TestE2E_Scenario3_MultipleGranularCommits tests Claude making multiple granular commits
// during a single turn.
//
// Scenario 3: Claude Makes Multiple Granular Commits
// - Claude is instructed to make granular commits
// - Multiple commits happen during one turn
// - Each commit gets its own unique checkpoint ID
// - All checkpoints are finalized together at turn end
func TestE2E_Scenario3_MultipleGranularCommits(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Count commits before
	commitsBefore := env.GetCommitCount()
	t.Logf("Commits before: %d", commitsBefore)

	// Agent creates multiple files and commits them separately
	granularCommitPrompt := `Please do the following tasks, committing after each one:

1. Create a file called file1.go with this content:
   package main
   func One() int { return 1 }
   Then run: git add file1.go && git commit -m "Add file1"

2. Create a file called file2.go with this content:
   package main
   func Two() int { return 2 }
   Then run: git add file2.go && git commit -m "Add file2"

3. Create a file called file3.go with this content:
   package main
   func Three() int { return 3 }
   Then run: git add file3.go && git commit -m "Add file3"

Do each task in order, making the commit after each file creation.`

	result, err := env.RunAgentWithTools(granularCommitPrompt, []string{"Write", "Bash"})
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)
	t.Logf("Agent output: %s", result.Stdout)

	// Verify all files were created
	assert.True(t, env.FileExists("file1.go"), "file1.go should exist")
	assert.True(t, env.FileExists("file2.go"), "file2.go should exist")
	assert.True(t, env.FileExists("file3.go"), "file3.go should exist")

	// Verify multiple commits were made
	commitsAfter := env.GetCommitCount()
	t.Logf("Commits after: %d", commitsAfter)
	assert.GreaterOrEqual(t, commitsAfter-commitsBefore, 3, "Should have at least 3 new commits")

	// Get all checkpoint IDs from history
	checkpointIDs := env.GetAllCheckpointIDsFromHistory()
	t.Logf("Found %d checkpoint IDs in commit history", len(checkpointIDs))
	for i, id := range checkpointIDs {
		t.Logf("  Checkpoint %d: %s", i, id)
	}

	// Each commit should have its own unique checkpoint ID (1:1 model)
	if len(checkpointIDs) >= 3 {
		// Verify checkpoint IDs are unique
		idSet := make(map[string]bool)
		for _, id := range checkpointIDs {
			require.Falsef(t, idSet[id], "Checkpoint IDs should be unique: %s is duplicated", id)
			idSet[id] = true
		}
	}

	// Verify metadata branch exists
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist")

	// Validate each checkpoint has proper metadata and content
	// Note: checkpointIDs are in reverse chronological order (newest first)
	// So checkpointIDs[0] = file3.go, [1] = file2.go, [2] = file1.go
	//
	// With deferred finalization, all checkpoints from the same turn get the
	// FULL transcript at turn end, so all checkpoints should contain all file names.
	allFiles := []string{"file1.go", "file2.go", "file3.go"}
	for i, cpID := range checkpointIDs {
		fileNum := len(checkpointIDs) - i // Reverse the index to match file numbers
		fileName := fmt.Sprintf("file%d.go", fileNum)
		t.Logf("Validating checkpoint %d: %s (files_touched: %s)", i, cpID, fileName)
		env.ValidateCheckpoint(CheckpointValidation{
			CheckpointID:              cpID,
			Strategy:                  "manual-commit",
			FilesTouched:              []string{fileName},
			ExpectedTranscriptContent: allFiles, // All checkpoints have full transcript
		})
	}
}

// TestE2E_Scenario4_UserSplitsCommits tests user splitting agent changes into multiple commits.
//
// Scenario 4: User Splits Changes Into Multiple Commits
// - Agent makes changes to multiple files
// - User commits only some files first
// - Uncommitted files are carried forward
// - User commits remaining files later
// - Each commit gets its own checkpoint ID
func TestE2E_Scenario4_UserSplitsCommits(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Agent creates multiple files in one prompt
	multiFilePrompt := `Create these files:
1. fileA.go with content: package main; func A() string { return "A" }
2. fileB.go with content: package main; func B() string { return "B" }
3. fileC.go with content: package main; func C() string { return "C" }
4. fileD.go with content: package main; func D() string { return "D" }

Create all four files, no other files or actions.`

	result, err := env.RunAgent(multiFilePrompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Verify all files were created
	assert.True(t, env.FileExists("fileA.go"), "fileA.go should exist")
	assert.True(t, env.FileExists("fileB.go"), "fileB.go should exist")
	assert.True(t, env.FileExists("fileC.go"), "fileC.go should exist")
	assert.True(t, env.FileExists("fileD.go"), "fileD.go should exist")

	// Check rewind points before commit
	pointsBefore := env.GetRewindPoints()
	t.Logf("Rewind points before commit: %d", len(pointsBefore))

	// User commits only A, B first
	t.Log("Committing fileA.go and fileB.go only")
	env.GitCommitWithShadowHooks("Add files A and B", "fileA.go", "fileB.go")

	// Verify first checkpoint was created
	checkpointIDsAfterFirstCommit := env.GetAllCheckpointIDsFromHistory()
	require.GreaterOrEqual(t, len(checkpointIDsAfterFirstCommit), 1, "Should have first checkpoint")
	// Note: GetAllCheckpointIDsFromHistory returns IDs in reverse chronological order (newest first)
	checkpointAB := checkpointIDsAfterFirstCommit[0]
	t.Logf("Checkpoint for A,B commit: %s", checkpointAB)

	// Check rewind points - should still have points for uncommitted files
	pointsAfterFirst := env.GetRewindPoints()
	t.Logf("Rewind points after first commit: %d", len(pointsAfterFirst))

	// User commits C, D later
	t.Log("Committing fileC.go and fileD.go")
	env.GitCommitWithShadowHooks("Add files C and D", "fileC.go", "fileD.go")

	// Verify second checkpoint was created with different ID
	checkpointIDsAfterSecondCommit := env.GetAllCheckpointIDsFromHistory()
	require.GreaterOrEqual(t, len(checkpointIDsAfterSecondCommit), 2, "Should have two checkpoints")
	checkpointCD := checkpointIDsAfterSecondCommit[0] // Most recent (C,D commit) is first
	t.Logf("Checkpoint for C,D commit: %s", checkpointCD)

	// Each commit should have its own unique checkpoint ID (1:1 model)
	assert.NotEqual(t, checkpointAB, checkpointCD,
		"Each commit should have its own unique checkpoint ID")

	// Verify metadata branch exists
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist")

	// Both checkpoints are from the same session where agent created all 4 files.
	// The transcript should contain all file names since it's the same agent work.
	allFiles := []string{"fileA.go", "fileB.go", "fileC.go", "fileD.go"}

	// Validate first checkpoint (files A, B committed)
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointAB,
		Strategy:                  "manual-commit",
		FilesTouched:              []string{"fileA.go", "fileB.go"},
		ExpectedTranscriptContent: allFiles, // Full session transcript
	})

	// Validate second checkpoint (files C, D committed)
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointCD,
		Strategy:                  "manual-commit",
		FilesTouched:              []string{"fileC.go", "fileD.go"},
		ExpectedTranscriptContent: allFiles, // Full session transcript
	})
}

// TestE2E_Scenario5_PartialCommitStashNextPrompt tests partial commit, stash, then new prompt.
//
// Scenario 5: Partial Commit → Stash → Next Prompt
// - Agent makes changes to A, B, C
// - User commits A only
// - User stashes B, C
// - User runs another prompt (creates D, E)
// - User commits D, E
// - FilesTouched accumulates across prompts
func TestE2E_Scenario5_PartialCommitStashNextPrompt(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Prompt 1: Agent creates files A, B, C
	t.Log("Prompt 1: Creating files A, B, C")
	prompt1 := `Create these files:
1. stash_a.go with content: package main; func StashA() {}
2. stash_b.go with content: package main; func StashB() {}
3. stash_c.go with content: package main; func StashC() {}
Create all three files, nothing else.`

	result, err := env.RunAgent(prompt1)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	require.True(t, env.FileExists("stash_a.go"))
	require.True(t, env.FileExists("stash_b.go"))
	require.True(t, env.FileExists("stash_c.go"))

	// User commits A only
	t.Log("Committing stash_a.go only")
	env.GitCommitWithShadowHooks("Add stash_a", "stash_a.go")

	// User stashes B, C
	t.Log("Stashing remaining files")
	env.GitStash()

	// Verify B, C are no longer in working directory
	assert.False(t, env.FileExists("stash_b.go"), "stash_b.go should be stashed")
	assert.False(t, env.FileExists("stash_c.go"), "stash_c.go should be stashed")

	// Prompt 2: Agent creates files D, E
	t.Log("Prompt 2: Creating files D, E")
	prompt2 := `Create these files:
1. stash_d.go with content: package main; func StashD() {}
2. stash_e.go with content: package main; func StashE() {}
Create both files, nothing else.`

	result, err = env.RunAgent(prompt2)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	require.True(t, env.FileExists("stash_d.go"))
	require.True(t, env.FileExists("stash_e.go"))

	// User commits D, E
	t.Log("Committing stash_d.go and stash_e.go")
	env.GitCommitWithShadowHooks("Add stash_d and stash_e", "stash_d.go", "stash_e.go")

	// Verify checkpoint was created for D, E
	checkpointIDs := env.GetAllCheckpointIDsFromHistory()
	require.GreaterOrEqual(t, len(checkpointIDs), 2, "Should have checkpoints for both commits")
	t.Logf("Checkpoint IDs: %v", checkpointIDs)

	// Verify metadata branch exists
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist")

	// Validate checkpoints have proper metadata and transcripts
	// checkpointIDs[0] is the most recent (D, E commit from prompt 2)
	// checkpointIDs[1] is the earlier commit (A only from prompt 1)
	//
	// These are from DIFFERENT sessions (prompt 1 vs prompt 2), so each has
	// its own transcript. Prompt 1 created A, B, C (B, C were stashed).
	// Prompt 2 created D, E.
	if len(checkpointIDs) >= 2 {
		env.ValidateCheckpoint(CheckpointValidation{
			CheckpointID:              checkpointIDs[0],
			Strategy:                  "manual-commit",
			FilesTouched:              []string{"stash_d.go", "stash_e.go"},
			ExpectedTranscriptContent: []string{"stash_d.go", "stash_e.go"}, // Prompt 2 transcript
		})
		env.ValidateCheckpoint(CheckpointValidation{
			CheckpointID:              checkpointIDs[1],
			Strategy:                  "manual-commit",
			FilesTouched:              []string{"stash_a.go"},
			ExpectedTranscriptContent: []string{"stash_a.go", "stash_b.go", "stash_c.go"}, // Full prompt 1 transcript
		})
	}
}

// TestE2E_Scenario6_StashSecondPromptUnstashCommitAll tests stash, new prompt, unstash, commit all.
//
// Scenario 6: Stash → Second Prompt → Unstash → Commit All
// - Agent makes changes to A, B, C
// - User commits A only
// - User stashes B, C
// - User runs another prompt (creates D, E)
// - User unstashes B, C
// - User commits ALL (B, C, D, E) together
// - All files link to single checkpoint
func TestE2E_Scenario6_StashSecondPromptUnstashCommitAll(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Prompt 1: Agent creates files A, B, C
	t.Log("Prompt 1: Creating files A, B, C")
	prompt1 := `Create these files:
1. combo_a.go with content: package main; func ComboA() {}
2. combo_b.go with content: package main; func ComboB() {}
3. combo_c.go with content: package main; func ComboC() {}
Create all three files, nothing else.`

	result, err := env.RunAgent(prompt1)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	require.True(t, env.FileExists("combo_a.go"))
	require.True(t, env.FileExists("combo_b.go"))
	require.True(t, env.FileExists("combo_c.go"))

	// User commits A only
	t.Log("Committing combo_a.go only")
	env.GitCommitWithShadowHooks("Add combo_a", "combo_a.go")

	// User stashes B, C
	t.Log("Stashing remaining files B, C")
	env.GitStash()

	// Verify B, C are no longer in working directory
	assert.False(t, env.FileExists("combo_b.go"), "combo_b.go should be stashed")
	assert.False(t, env.FileExists("combo_c.go"), "combo_c.go should be stashed")

	// Prompt 2: Agent creates files D, E
	t.Log("Prompt 2: Creating files D, E")
	prompt2 := `Create these files:
1. combo_d.go with content: package main; func ComboD() {}
2. combo_e.go with content: package main; func ComboE() {}
Create both files, nothing else.`

	result, err = env.RunAgent(prompt2)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	require.True(t, env.FileExists("combo_d.go"))
	require.True(t, env.FileExists("combo_e.go"))

	// User unstashes B, C
	t.Log("Unstashing B, C")
	env.GitStashPop()

	// Verify B, C are back
	assert.True(t, env.FileExists("combo_b.go"), "combo_b.go should be back after unstash")
	assert.True(t, env.FileExists("combo_c.go"), "combo_c.go should be back after unstash")

	// User commits ALL files together (B, C, D, E)
	t.Log("Committing all remaining files together")
	env.GitCommitWithShadowHooks("Add combo_b, combo_c, combo_d, combo_e",
		"combo_b.go", "combo_c.go", "combo_d.go", "combo_e.go")

	// Verify checkpoint was created
	checkpointIDs := env.GetAllCheckpointIDsFromHistory()
	require.GreaterOrEqual(t, len(checkpointIDs), 2, "Should have checkpoints")
	t.Logf("Checkpoint IDs: %v", checkpointIDs)

	// The second commit should have all 4 files linked to a single checkpoint
	// Verify metadata branch exists
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist")

	// Validate checkpoints have proper metadata and transcripts
	// checkpointIDs[0] is the most recent (B, C, D, E combined commit)
	// checkpointIDs[1] is the earlier commit (A only)
	//
	// Prompt 1 created A, B, C. User committed A, then stashed B, C.
	// Prompt 2 created D, E. User unstashed B, C, then committed all 4 together.
	if len(checkpointIDs) >= 2 {
		// The BCDE commit happens during prompt 2's session, so its transcript
		// contains prompt 2's work (D, E). B, C are included via carry-forward.
		env.ValidateCheckpoint(CheckpointValidation{
			CheckpointID:              checkpointIDs[0],
			Strategy:                  "manual-commit",
			FilesTouched:              []string{"combo_b.go", "combo_c.go", "combo_d.go", "combo_e.go"},
			ExpectedTranscriptContent: []string{"combo_d.go", "combo_e.go"}, // Prompt 2 transcript
		})
		// The A commit has full prompt 1 transcript (A, B, C were all created)
		env.ValidateCheckpoint(CheckpointValidation{
			CheckpointID:              checkpointIDs[1],
			Strategy:                  "manual-commit",
			FilesTouched:              []string{"combo_a.go"},
			ExpectedTranscriptContent: []string{"combo_a.go", "combo_b.go", "combo_c.go"}, // Full prompt 1 transcript
		})
	}
}

// TestE2E_Scenario7_PartialStagingWithGitAddP tests partial staging with git add -p.
//
// Scenario 7: Partial Staging with `git add -p`
// - Agent makes multiple changes to a single file
// - User stages only part of the changes (simulated with partial write)
// - Content-aware carry-forward detects partial commit
// - Remaining changes carried forward to next commit
func TestE2E_Scenario7_PartialStagingSimulated(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Agent creates a file with multiple functions
	t.Log("Creating file with multiple functions")
	multiLinePrompt := `Create a file called partial.go with this exact content:
package main

func First() int {
	return 1
}

func Second() int {
	return 2
}

func Third() int {
	return 3
}

func Fourth() int {
	return 4
}

Create only this file with exactly this content.`

	result, err := env.RunAgent(multiLinePrompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	require.True(t, env.FileExists("partial.go"))

	// Check rewind points before commit
	pointsBefore := env.GetRewindPoints()
	t.Logf("Rewind points before commit: %d", len(pointsBefore))

	// Simulate partial staging by temporarily replacing file with partial content
	// Save the full content
	fullContent := env.ReadFile("partial.go")
	t.Logf("Full content length: %d bytes", len(fullContent))

	// Write partial content (only first two functions)
	partialContent := `package main

func First() int {
	return 1
}

func Second() int {
	return 2
}
`
	env.WriteFile("partial.go", partialContent)

	// Commit the partial content
	t.Log("Committing partial content (First and Second functions)")
	env.GitCommitWithShadowHooks("Add first two functions", "partial.go")

	// Verify first checkpoint was created
	checkpointIDs := env.GetAllCheckpointIDsFromHistory()
	require.GreaterOrEqual(t, len(checkpointIDs), 1, "Should have first checkpoint")
	t.Logf("First checkpoint ID: %s", checkpointIDs[0])

	// Restore the full content (simulating remaining changes still in worktree)
	env.WriteFile("partial.go", fullContent)

	// Commit the remaining content
	t.Log("Committing full content (all functions)")
	env.GitCommitWithShadowHooks("Add remaining functions", "partial.go")

	// Verify second checkpoint was created
	checkpointIDsAfter := env.GetAllCheckpointIDsFromHistory()
	require.GreaterOrEqual(t, len(checkpointIDsAfter), 2, "Should have two checkpoints")
	t.Logf("Checkpoint IDs: %v", checkpointIDsAfter)

	// Each commit should have its own unique checkpoint ID
	assert.NotEqual(t, checkpointIDsAfter[0], checkpointIDsAfter[1],
		"Each commit should have its own unique checkpoint ID")

	// Validate checkpoints have proper metadata and transcripts
	// checkpointIDsAfter[0] is the most recent (full content commit)
	// checkpointIDsAfter[1] is the earlier commit (partial content)
	//
	// Both commits are from the same session (single prompt), so both have
	// the same full transcript referencing partial.go and the function names.
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointIDsAfter[0],
		Strategy:                  "manual-commit",
		FilesTouched:              []string{"partial.go"},
		ExpectedTranscriptContent: []string{"partial.go", "First", "Second", "Third", "Fourth"},
	})
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointIDsAfter[1],
		Strategy:                  "manual-commit",
		FilesTouched:              []string{"partial.go"},
		ExpectedTranscriptContent: []string{"partial.go", "First", "Second", "Third", "Fourth"},
	})
}

// TestE2E_ContentAwareOverlap_RevertAndReplace tests content-aware overlap detection
// when user reverts session changes and writes different content.
//
// Content-Aware Overlap Detection:
// - Agent creates file X with content "hello"
// - User reverts X (git checkout -- X)
// - User writes completely different content
// - User commits
// - Content mismatch → NO checkpoint trailer added
func TestE2E_ContentAwareOverlap_RevertAndReplace(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// Agent creates a file
	t.Log("Agent creating file with specific content")
	createPrompt := `Create a file called overlap_test.go with this exact content:
package main

func OverlapOriginal() string {
	return "original content from agent"
}

Create only this file.`

	result, err := env.RunAgent(createPrompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	require.True(t, env.FileExists("overlap_test.go"))
	originalContent := env.ReadFile("overlap_test.go")
	t.Logf("Original content: %s", originalContent)

	// Verify rewind points exist (session tracked the change)
	points := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(points), 1, "Should have rewind points")

	// User reverts the file and writes completely different content
	t.Log("User reverting file and writing different content")
	differentContent := `package main

func CompletelyDifferent() string {
	return "user wrote this, not the agent"
}
`
	env.WriteFile("overlap_test.go", differentContent)

	// Verify content is different
	currentContent := env.ReadFile("overlap_test.go")
	assert.NotEqual(t, originalContent, currentContent)

	// Commits before this test
	commitsBefore := env.GetCommitCount()

	// User commits the different content
	t.Log("Committing user-written content")
	env.GitCommitWithShadowHooks("Add overlap test file", "overlap_test.go")

	// Verify commit was made
	commitsAfter := env.GetCommitCount()
	assert.Equal(t, commitsBefore+1, commitsAfter, "Should have one new commit")

	// Check for checkpoint trailer
	// With content-aware overlap detection, if the user completely replaced
	// the agent's content (new file with different hash), there should be
	// NO checkpoint trailer added. This is documented in checkpoint-scenarios.md
	// under "Content-Aware Overlap Detection".
	checkpointIDs := env.GetAllCheckpointIDsFromHistory()
	t.Logf("Checkpoint IDs found: %v", checkpointIDs)

	// The first commit (Initial commit from NewFeatureBranchEnv) doesn't have a checkpoint.
	// Only agent-assisted commits should have checkpoint trailers.
	// When user completely replaces agent content, content-aware detection should
	// prevent the checkpoint trailer from being added.
	//
	// Per docs/architecture/checkpoint-scenarios.md:
	// - New file + content hash mismatch → No overlap → No checkpoint trailer
	assert.Empty(t, checkpointIDs,
		"Content-aware detection should prevent checkpoint trailer when user completely replaces agent content")
}

// TestE2E_Scenario1_BasicFlow verifies the simplest workflow matches the documented scenario.
//
// Scenario 1: Prompt → Changes → Prompt Finishes → User Commits
// This test explicitly verifies the documented flow.
func TestE2E_Scenario1_BasicFlow(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	// 1. User submits prompt (triggers UserPromptSubmit hook → InitializeSession)
	t.Log("Step 1: User submits prompt")

	// 2. Claude makes changes (creates files)
	prompt := `Create a file called scenario1.go with this content:
package main
func Scenario1() {}
Create only this file.`

	result, err := env.RunAgent(prompt)
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Verify file was created
	require.True(t, env.FileExists("scenario1.go"))

	// 3. After stop hook: checkpoint is saved, FilesTouched is set
	t.Log("Step 3: Checking rewind points after stop")
	points := env.GetRewindPoints()
	require.GreaterOrEqual(t, len(points), 1, "Should have rewind point after stop")
	t.Logf("Rewind points: %d", len(points))

	// 4. User commits (triggers prepare-commit-msg and post-commit hooks)
	t.Log("Step 4: User commits")
	env.GitCommitWithShadowHooks("Add scenario1 file", "scenario1.go")

	// 5. Verify checkpoint was created with trailer
	checkpointID, err := env.GetLatestCheckpointIDFromHistory()
	require.NoError(t, err, "Should find checkpoint in commit history")
	assert.NotEmpty(t, checkpointID, "Commit should have Entire-Checkpoint trailer")
	t.Logf("Checkpoint ID: %s", checkpointID)

	// 6. Verify shadow branch was cleaned up and metadata branch exists
	assert.True(t, env.BranchExists("entire/checkpoints/v1"),
		"entire/checkpoints/v1 branch should exist after condensation")

	// 7. Validate checkpoint has proper metadata and transcript
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointID,
		Strategy:                  "manual-commit",
		FilesTouched:              []string{"scenario1.go"},
		ExpectedTranscriptContent: []string{"scenario1.go"},
	})
}

// TestE2E_Scenario2_AgentCommitsDuringTurn verifies the deferred finalization flow.
//
// Scenario 2: Prompt Commits Within Single Turn
// - Agent commits during ACTIVE phase
// - PostCommit saves provisional transcript
// - HandleTurnEnd (Stop hook) finalizes with full transcript
func TestE2E_Scenario2_AgentCommitsDuringTurn(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t, "manual-commit")

	commitsBefore := env.GetCommitCount()

	// Agent creates file and commits it
	t.Log("Agent creating file and committing")
	commitPrompt := `Create a file called agent_commit.go with this content:
package main
func AgentCommit() {}

Then commit it with: git add agent_commit.go && git commit -m "Agent adds file"

Create the file first, then run the git commands.`

	result, err := env.RunAgentWithTools(commitPrompt, []string{"Write", "Bash"})
	require.NoError(t, err)
	AssertAgentSuccess(t, result, err)

	// Verify file was created
	require.True(t, env.FileExists("agent_commit.go"))

	// Verify commit was made
	commitsAfter := env.GetCommitCount()
	assert.Greater(t, commitsAfter, commitsBefore, "Agent should have made a commit")

	// Check commit message
	headMsg := env.GetCommitMessage(env.GetHeadHash())
	t.Logf("HEAD commit message: %s", headMsg)

	// Check for checkpoint trailer
	checkpointIDs := env.GetAllCheckpointIDsFromHistory()
	t.Logf("Checkpoint IDs: %v", checkpointIDs)

	// Verify metadata branch exists (if checkpoint was created)
	if len(checkpointIDs) > 0 {
		assert.True(t, env.BranchExists("entire/checkpoints/v1"),
			"entire/checkpoints/v1 branch should exist")

		// Validate checkpoint has proper metadata and transcript
		env.ValidateCheckpoint(CheckpointValidation{
			CheckpointID:              checkpointIDs[0],
			Strategy:                  "manual-commit",
			FilesTouched:              []string{"agent_commit.go"},
			ExpectedTranscriptContent: []string{"agent_commit.go"},
		})
	}
}
