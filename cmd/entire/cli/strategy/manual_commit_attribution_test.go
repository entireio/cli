package strategy

import (
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

const testThreeLines = "line1\nline2\nline3\n"

func TestDiffLines_NoChanges(t *testing.T) {
	content := testThreeLines
	unchanged, added, removed := diffLines(content, content)

	if unchanged != 3 {
		t.Errorf("expected 3 unchanged lines, got %d", unchanged)
	}
	if added != 0 {
		t.Errorf("expected 0 added lines, got %d", added)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed lines, got %d", removed)
	}
}

func TestDiffLines_AllAdded(t *testing.T) {
	checkpoint := ""
	committed := testThreeLines
	unchanged, added, removed := diffLines(checkpoint, committed)

	if unchanged != 0 {
		t.Errorf("expected 0 unchanged lines, got %d", unchanged)
	}
	if added != 3 {
		t.Errorf("expected 3 added lines, got %d", added)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed lines, got %d", removed)
	}
}

func TestDiffLines_AllRemoved(t *testing.T) {
	checkpoint := testThreeLines
	committed := ""
	unchanged, added, removed := diffLines(checkpoint, committed)

	if unchanged != 0 {
		t.Errorf("expected 0 unchanged lines, got %d", unchanged)
	}
	if added != 0 {
		t.Errorf("expected 0 added lines, got %d", added)
	}
	if removed != 3 {
		t.Errorf("expected 3 removed lines, got %d", removed)
	}
}

func TestDiffLines_MixedChanges(t *testing.T) {
	checkpoint := testThreeLines
	committed := "line1\nmodified\nline3\nnew line\n"
	unchanged, added, removed := diffLines(checkpoint, committed)

	// line1 and line3 unchanged (2)
	// line2 removed (1)
	// modified and new line added (2)
	if unchanged != 2 {
		t.Errorf("expected 2 unchanged lines, got %d", unchanged)
	}
	if added != 2 {
		t.Errorf("expected 2 added lines, got %d", added)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed line, got %d", removed)
	}
}

func TestDiffLines_WithoutTrailingNewline(t *testing.T) {
	checkpoint := "line1\nline2"
	committed := "line1\nline2"
	unchanged, added, removed := diffLines(checkpoint, committed)

	if unchanged != 2 {
		t.Errorf("expected 2 unchanged lines, got %d", unchanged)
	}
	if added != 0 {
		t.Errorf("expected 0 added lines, got %d", added)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed lines, got %d", removed)
	}
}

func TestCountLinesStr(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected int
	}{
		{"empty", "", 0},
		{"single line no newline", "hello", 1},
		{"single line with newline", "hello\n", 1},
		{"two lines", "hello\nworld\n", 2},
		{"two lines no trailing newline", "hello\nworld", 2},
		{"three lines", "a\nb\nc\n", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countLinesStr(tt.content)
			if got != tt.expected {
				t.Errorf("countLinesStr(%q) = %d, want %d", tt.content, got, tt.expected)
			}
		})
	}
}

func TestDiffLines_PercentageCalculation(t *testing.T) {
	// Test diffLines with a basic addition scenario
	checkpoint := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n"
	committed := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nnew1\nnew2\n"

	unchanged, added, removed := diffLines(checkpoint, committed)

	if unchanged != 8 {
		t.Errorf("expected 8 unchanged, got %d", unchanged)
	}
	if added != 2 {
		t.Errorf("expected 2 added, got %d", added)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}

	// Verify countLinesStr matches
	totalCommitted := countLinesStr(committed)
	if totalCommitted != 10 {
		t.Errorf("expected 10 total committed, got %d", totalCommitted)
	}
}

func TestDiffLines_ModifiedEstimation(t *testing.T) {
	// Test diffLines with modifications (additions + removals)
	// When we have both additions and removals, min(added, removed) represents modifications
	checkpoint := "original1\noriginal2\noriginal3\n"
	committed := "modified1\nmodified2\noriginal3\nnew line\n"

	unchanged, added, removed := diffLines(checkpoint, committed)

	// original3 is unchanged (1)
	// original1, original2 removed (2)
	// modified1, modified2, new line added (3)
	if unchanged != 1 {
		t.Errorf("expected 1 unchanged, got %d", unchanged)
	}
	if added != 3 {
		t.Errorf("expected 3 added, got %d", added)
	}
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	// Estimate modified lines: min(3, 2) = 2 modified
	// humanModified = 2
	// humanAdded = 3 - 2 = 1 (pure additions)
	// humanRemoved = 2 - 2 = 0 (pure removals)
	humanModified := min(added, removed)
	humanAdded := added - humanModified
	humanRemoved := removed - humanModified

	if humanModified != 2 {
		t.Errorf("expected 2 modified, got %d", humanModified)
	}
	if humanAdded != 1 {
		t.Errorf("expected 1 pure added (after subtracting modified), got %d", humanAdded)
	}
	if humanRemoved != 0 {
		t.Errorf("expected 0 pure removed (after subtracting modified), got %d", humanRemoved)
	}
}

// buildTestTree creates an object.Tree from a map of file paths to content.
// This is a test helper for creating trees without a full git repository.
func buildTestTree(t *testing.T, files map[string]string) *object.Tree {
	t.Helper()

	if len(files) == 0 {
		return nil
	}

	// Use memory storage to build a tree
	storage := memory.NewStorage()

	// Create blob objects for each file
	var entries []object.TreeEntry
	for path, content := range files {
		// Encode the blob
		obj := storage.NewEncodedObject()
		obj.SetType(plumbing.BlobObject)
		writer, err := obj.Writer()
		if err != nil {
			t.Fatalf("failed to create blob writer: %v", err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatalf("failed to write blob content: %v", err)
		}
		writer.Close()

		// Store the blob
		hash, err := storage.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("failed to store blob: %v", err)
		}

		// Create tree entry
		entries = append(entries, object.TreeEntry{
			Name: path,
			Mode: 0o100644,
			Hash: hash,
		})
	}

	// Sort entries by name (required by git tree format)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})

	// Create the tree
	tree := &object.Tree{
		Entries: entries,
	}

	// Encode and store the tree
	obj := storage.NewEncodedObject()
	obj.SetType(plumbing.TreeObject)
	if err := tree.Encode(obj); err != nil {
		t.Fatalf("failed to encode tree: %v", err)
	}

	hash, err := storage.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("failed to store tree: %v", err)
	}

	// Retrieve the tree
	treeObj, err := object.GetTree(storage, hash)
	if err != nil {
		t.Fatalf("failed to get tree: %v", err)
	}

	return treeObj
}

// TestCalculateAttributionWithAccumulated_BasicCase tests the basic scenario
// where the agent adds lines and the user makes some edits.
//
//nolint:dupl // Test structure is similar but validates different scenarios
func TestCalculateAttributionWithAccumulated_BasicCase(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow (agent work): agent adds 8 lines
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n",
	})

	// Head (final commit): user added 2 more lines
	headTree := buildTestTree(t, map[string]string{
		"main.go": "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nuser1\nuser2\n",
	})

	filesTouched := []string{"main.go"}
	promptAttributions := []PromptAttribution{} // No intermediate checkpoints

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected:
	// - Agent added 8 lines (base → shadow)
	// - User added 2 lines (shadow → head)
	// - No removals or modifications
	// - Total = 8 + 2 = 10
	// - Agent percentage = 8/10 = 80%

	if result.AgentLines != 8 {
		t.Errorf("AgentLines = %d, want 8", result.AgentLines)
	}
	if result.HumanAdded != 2 {
		t.Errorf("HumanAdded = %d, want 2", result.HumanAdded)
	}
	if result.HumanModified != 0 {
		t.Errorf("HumanModified = %d, want 0", result.HumanModified)
	}
	if result.HumanRemoved != 0 {
		t.Errorf("HumanRemoved = %d, want 0", result.HumanRemoved)
	}
	if result.TotalCommitted != 10 {
		t.Errorf("TotalCommitted = %d, want 10", result.TotalCommitted)
	}
	if result.AgentPercentage < 79.9 || result.AgentPercentage > 80.1 {
		t.Errorf("AgentPercentage = %.1f%%, want 80.0%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_BugScenario tests the specific bug case:
// agent adds 10 lines, user removes 5 and adds 2.
//
//nolint:dupl // Test structure is similar but validates different scenarios
func TestCalculateAttributionWithAccumulated_BugScenario(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow (agent work): agent adds 10 lines
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nagent9\nagent10\n",
	})

	// Head (final commit): user removed 5 agent lines and added 2 new lines
	headTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\nuser1\nuser2\n",
	})

	filesTouched := []string{"main.go"}
	promptAttributions := []PromptAttribution{} // No intermediate checkpoints

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected:
	// - Agent added 10 lines (base → shadow)
	// - User added 2 lines, removed 5 lines (shadow → head)
	// - humanModified = min(2, 5) = 2
	// - pureUserAdded = 2 - 2 = 0
	// - pureUserRemoved = 5 - 2 = 3
	// - agentLinesInCommit = 10 - 3 - 2 = 5
	// - Total = 10 + 0 - 3 = 7
	// - Agent percentage = 5/7 = 71.4%

	if result.AgentLines != 5 {
		t.Errorf("AgentLines = %d, want 5 (10 added - 3 removed - 2 modified)", result.AgentLines)
	}
	if result.HumanAdded != 0 {
		t.Errorf("HumanAdded = %d, want 0 (2 additions counted as modifications)", result.HumanAdded)
	}
	if result.HumanModified != 2 {
		t.Errorf("HumanModified = %d, want 2 (min of 2 added, 5 removed)", result.HumanModified)
	}
	if result.HumanRemoved != 3 {
		t.Errorf("HumanRemoved = %d, want 3 (5 removed - 2 modifications)", result.HumanRemoved)
	}
	if result.TotalCommitted != 7 {
		t.Errorf("TotalCommitted = %d, want 7 (10 agent + 0 pure user added - 3 pure user removed)", result.TotalCommitted)
	}
	if result.AgentPercentage < 71.0 || result.AgentPercentage > 72.0 {
		t.Errorf("AgentPercentage = %.1f%%, want ~71.4%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_DeletionOnly tests a deletion-only commit.
func TestCalculateAttributionWithAccumulated_DeletionOnly(t *testing.T) {
	// Base: file with content
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "line1\nline2\nline3\nline4\nline5\n",
	})

	// Shadow (agent work): agent removes 2 lines
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "line1\nline2\nline3\n",
	})

	// Head (final commit): user removes 2 more lines
	headTree := buildTestTree(t, map[string]string{
		"main.go": "line1\n",
	})

	filesTouched := []string{"main.go"}
	promptAttributions := []PromptAttribution{} // No intermediate checkpoints

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected:
	// - Agent added 0 lines (only deletions)
	// - User removed 2 lines (shadow → head)
	// - Total = 0 (fallback to totalAgentAdded which is 0)
	// - Agent percentage = 0

	if result.AgentLines != 0 {
		t.Errorf("AgentLines = %d, want 0 (deletion-only)", result.AgentLines)
	}
	if result.HumanAdded != 0 {
		t.Errorf("HumanAdded = %d, want 0", result.HumanAdded)
	}
	if result.HumanRemoved != 2 {
		t.Errorf("HumanRemoved = %d, want 2", result.HumanRemoved)
	}
	if result.TotalCommitted != 0 {
		t.Errorf("TotalCommitted = %d, want 0 (deletion-only)", result.TotalCommitted)
	}
	if result.AgentPercentage != 0 {
		t.Errorf("AgentPercentage = %.1f%%, want 0.0%% (deletion-only)", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_NoUserEdits tests when user makes no changes.
func TestCalculateAttributionWithAccumulated_NoUserEdits(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow and Head are identical (no user edits after agent)
	content := "agent1\nagent2\nagent3\nagent4\nagent5\n"
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": content,
	})
	headTree := buildTestTree(t, map[string]string{
		"main.go": content,
	})

	filesTouched := []string{"main.go"}
	promptAttributions := []PromptAttribution{} // No intermediate checkpoints

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected:
	// - Agent added 5 lines
	// - No user edits
	// - Total = 5
	// - Agent percentage = 100%

	if result.AgentLines != 5 {
		t.Errorf("AgentLines = %d, want 5", result.AgentLines)
	}
	if result.HumanAdded != 0 {
		t.Errorf("HumanAdded = %d, want 0", result.HumanAdded)
	}
	if result.HumanModified != 0 {
		t.Errorf("HumanModified = %d, want 0", result.HumanModified)
	}
	if result.HumanRemoved != 0 {
		t.Errorf("HumanRemoved = %d, want 0", result.HumanRemoved)
	}
	if result.TotalCommitted != 5 {
		t.Errorf("TotalCommitted = %d, want 5", result.TotalCommitted)
	}
	if result.AgentPercentage != 100.0 {
		t.Errorf("AgentPercentage = %.1f%%, want 100.0%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_NoAgentWork tests when agent makes no changes.
func TestCalculateAttributionWithAccumulated_NoAgentWork(t *testing.T) {
	// Base and Shadow are identical (no agent work)
	content := "line1\nline2\nline3\n"
	baseTree := buildTestTree(t, map[string]string{
		"main.go": content,
	})
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": content,
	})

	// Head: user added 2 lines
	headTree := buildTestTree(t, map[string]string{
		"main.go": content + "user1\nuser2\n",
	})

	filesTouched := []string{"main.go"}
	promptAttributions := []PromptAttribution{} // No intermediate checkpoints

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected:
	// - Agent added 0 lines
	// - User added 2 lines
	// - Total = 0 + 2 = 2
	// - Agent percentage = 0%

	if result.AgentLines != 0 {
		t.Errorf("AgentLines = %d, want 0", result.AgentLines)
	}
	if result.HumanAdded != 2 {
		t.Errorf("HumanAdded = %d, want 2", result.HumanAdded)
	}
	if result.HumanModified != 0 {
		t.Errorf("HumanModified = %d, want 0", result.HumanModified)
	}
	if result.HumanRemoved != 0 {
		t.Errorf("HumanRemoved = %d, want 0", result.HumanRemoved)
	}
	if result.TotalCommitted != 2 {
		t.Errorf("TotalCommitted = %d, want 2", result.TotalCommitted)
	}
	if result.AgentPercentage != 0.0 {
		t.Errorf("AgentPercentage = %.1f%%, want 0.0%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_UserRemovesAllAgentLines tests when
// the user removes all lines the agent added.
func TestCalculateAttributionWithAccumulated_UserRemovesAllAgentLines(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow (agent work): agent adds 5 lines
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\n",
	})

	// Head (final commit): user removed all agent lines and added their own
	headTree := buildTestTree(t, map[string]string{
		"main.go": "user1\nuser2\nuser3\n",
	})

	filesTouched := []string{"main.go"}
	promptAttributions := []PromptAttribution{} // No intermediate checkpoints

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected:
	// - Agent added 5 lines (base → shadow)
	// - User added 3 lines, removed 5 lines (shadow → head)
	// - humanModified = min(3, 5) = 3
	// - pureUserAdded = 3 - 3 = 0
	// - pureUserRemoved = 5 - 3 = 2
	// - agentLinesInCommit = 5 - 2 - 3 = 0
	// - Total = 5 + 0 - 2 = 3
	// - Agent percentage = 0/3 = 0%

	if result.AgentLines != 0 {
		t.Errorf("AgentLines = %d, want 0 (all agent lines removed/modified)", result.AgentLines)
	}
	if result.HumanAdded != 0 {
		t.Errorf("HumanAdded = %d, want 0 (all counted as modifications)", result.HumanAdded)
	}
	if result.HumanModified != 3 {
		t.Errorf("HumanModified = %d, want 3", result.HumanModified)
	}
	if result.HumanRemoved != 2 {
		t.Errorf("HumanRemoved = %d, want 2", result.HumanRemoved)
	}
	if result.TotalCommitted != 3 {
		t.Errorf("TotalCommitted = %d, want 3", result.TotalCommitted)
	}
	if result.AgentPercentage != 0.0 {
		t.Errorf("AgentPercentage = %.1f%%, want 0.0%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_WithPromptAttributions tests with
// accumulated user edits captured between checkpoints.
func TestCalculateAttributionWithAccumulated_WithPromptAttributions(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow (final checkpoint): includes agent work (10 lines) + user work between checkpoints (2 lines)
	// The shadow tree captures the worktree state, which includes user edits made between checkpoints
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nuser_between1\nuser_between2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nagent9\nagent10\n",
	})

	// Head (final commit): shadow + 1 more user line
	headTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nuser_between1\nuser_between2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nagent9\nagent10\nuser_after\n",
	})

	filesTouched := []string{"main.go"}

	// PromptAttribution captured that 2 lines were added by user between checkpoints
	// This helps separate user work from agent work, since shadow tree includes both
	promptAttributions := []PromptAttribution{
		{
			CheckpointNumber: 2,
			UserLinesAdded:   2, // user_between1, user_between2
			UserLinesRemoved: 0,
		},
	}

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected calculation:
	// - base → shadow: +12 lines added (includes agent + user between)
	// - shadow → head: +1 line added (user after)
	// - accumulatedUserAdded: 2 (from PromptAttributions)
	// - totalUserAdded: 2 + 1 = 3
	// - totalAgentAdded: 12 (base → shadow, includes mixed content)
	// - The function doesn't separate agent from user in the shadow snapshot,
	//   so totalAgentAdded is actually 12 (not 10), which is a known limitation
	// - agentLinesInCommit: 12
	// - Total: 12 + 3 = 15
	// - Agent percentage: 12/15 = 80%
	//
	// Note: This test documents current behavior. Ideally, totalAgentAdded would be 10
	// (excluding the 2 user lines), but the algorithm doesn't currently separate them.

	if result.AgentLines != 12 {
		t.Errorf("AgentLines = %d, want 12 (includes user lines in shadow snapshot)", result.AgentLines)
	}
	if result.HumanAdded != 3 {
		t.Errorf("HumanAdded = %d, want 3 (2 between + 1 after)", result.HumanAdded)
	}
	if result.HumanModified != 0 {
		t.Errorf("HumanModified = %d, want 0", result.HumanModified)
	}
	if result.HumanRemoved != 0 {
		t.Errorf("HumanRemoved = %d, want 0", result.HumanRemoved)
	}
	if result.TotalCommitted != 15 {
		t.Errorf("TotalCommitted = %d, want 15 (12 + 3)", result.TotalCommitted)
	}
	if result.AgentPercentage < 79.9 || result.AgentPercentage > 80.1 {
		t.Errorf("AgentPercentage = %.1f%%, want 80.0%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_EmptyFilesTouched tests with no files.
func TestCalculateAttributionWithAccumulated_EmptyFilesTouched(t *testing.T) {
	baseTree := buildTestTree(t, map[string]string{})
	shadowTree := buildTestTree(t, map[string]string{})
	headTree := buildTestTree(t, map[string]string{})

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, []string{}, []PromptAttribution{},
	)

	if result != nil {
		t.Errorf("expected nil result for empty filesTouched, got %+v", result)
	}
}

// TestCalculateAttributionWithAccumulated_UserEditsNonAgentFile tests the bug where
// post-checkpoint user edits to files the agent never touched are undercounted.
//
// Bug scenario:
// 1. Agent touches file1.go (added to filesTouched)
// 2. User edits file2.go between checkpoints → captured in PromptAttributions
// 3. User edits file2.go again AFTER last checkpoint, before commit
// 4. BUG: Post-checkpoint calculation only looks at filesTouched (file1.go),
//    missing the file2.go edits in step 3
//
// This causes undercounted user contributions and inflated agent percentage.
func TestCalculateAttributionWithAccumulated_UserEditsNonAgentFile(t *testing.T) {
	// Base: two files
	baseTree := buildTestTree(t, map[string]string{
		"file1.go": "package main\n",
		"file2.go": "package util\n",
	})

	// Shadow (agent work): agent adds to file1.go only
	shadowTree := buildTestTree(t, map[string]string{
		"file1.go": "package main\n\nfunc agent1() {}\nfunc agent2() {}\n",
		"file2.go": "package util\n\n// User edit 1\n// User edit 2\n",
	})

	// Head (final commit): user adds more to file2.go AFTER last checkpoint
	headTree := buildTestTree(t, map[string]string{
		"file1.go": "package main\n\nfunc agent1() {}\nfunc agent2() {}\n",
		"file2.go": "package util\n\n// User edit 1\n// User edit 2\n// User edit 3\n// User edit 4\n",
	})

	// filesTouched only includes file1.go (agent-touched)
	filesTouched := []string{"file1.go"}

	// PromptAttributions captured user edits to file2.go between checkpoints
	promptAttributions := []PromptAttribution{
		{
			CheckpointNumber: 1,
			UserLinesAdded:   2, // User edit 1, 2 (between checkpoints)
			UserLinesRemoved: 0,
		},
	}

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected calculation:
	// - Agent added 3 lines to file1.go (2 functions + 1 blank)
	// - User added 2 lines to file2.go between checkpoints (from PromptAttribution)
	// - User added 2 MORE lines to file2.go after last checkpoint (post-checkpoint)
	// - Total user added: 2 + 2 = 4
	// - agentLinesInCommit: 3
	// - Total: 3 + 4 = 7
	// - Agent percentage: 3/7 = 42.9%
	//
	// BUG (if not fixed): Post-checkpoint calculation only looks at file1.go,
	// so it would miss the 2 post-checkpoint edits to file2.go:
	// - Total user added: 2 + 0 = 2 (WRONG)
	// - Total: 3 + 2 = 5 (WRONG)
	// - Agent percentage: 3/5 = 60% (WRONG, inflated)

	t.Logf("Attribution: agent=%d, human_added=%d, total=%d, percentage=%.1f%%",
		result.AgentLines, result.HumanAdded, result.TotalCommitted, result.AgentPercentage)

	if result.AgentLines != 3 {
		t.Errorf("AgentLines = %d, want 3", result.AgentLines)
	}

	if result.HumanAdded != 4 {
		t.Errorf("HumanAdded = %d, want 4 (2 between + 2 post-checkpoint, including file agent never touched)",
			result.HumanAdded)
	}

	if result.TotalCommitted != 7 {
		t.Errorf("TotalCommitted = %d, want 7 (3 agent + 4 user)", result.TotalCommitted)
	}

	// Agent percentage should be 3/7 = 42.9%
	if result.AgentPercentage < 42.8 || result.AgentPercentage > 43.0 {
		t.Errorf("AgentPercentage = %.1f%%, want ~42.9%% (not inflated)", result.AgentPercentage)
	}
}
