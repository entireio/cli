package strategy

import (
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

const testThreeLines = "line1\nline2\nline3\n"
const testFile1 = "file1.go"

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
			UserAddedPerFile: map[string]int{"main.go": 2}, // User edited the agent-touched file
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
	// - totalAgentAdded: 12 - 2 = 10 (correctly separates user lines from agent work)
	// - totalUserAdded: 2 + 1 = 3
	// - agentLinesInCommit: 10
	// - Total: 10 + 3 = 13
	// - Agent percentage: 10/13 = 76.9%

	if result.AgentLines != 10 {
		t.Errorf("AgentLines = %d, want 10 (excludes user lines in shadow snapshot)", result.AgentLines)
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
	if result.TotalCommitted != 13 {
		t.Errorf("TotalCommitted = %d, want 13 (10 + 3)", result.TotalCommitted)
	}
	if result.AgentPercentage < 76.8 || result.AgentPercentage > 77.0 {
		t.Errorf("AgentPercentage = %.1f%%, want 76.9%%", result.AgentPercentage)
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
//  1. Agent touches file1.go (added to filesTouched)
//  2. User edits file2.go between checkpoints → captured in PromptAttributions
//  3. User edits file2.go again AFTER last checkpoint, before commit
//  4. BUG: Post-checkpoint calculation only looks at filesTouched (file1.go),
//     missing the file2.go edits in step 3
//
// This causes undercounted user contributions and inflated agent percentage.
func TestCalculateAttributionWithAccumulated_UserEditsNonAgentFile(t *testing.T) {
	// Base: two files
	baseTree := buildTestTree(t, map[string]string{
		"file1.go": "package main\n",
		"file2.go": "package util\n",
	})

	// Shadow (agent work): agent adds to file1.go only
	// file2.go is NOT in shadow tree because it's not in filesTouched
	shadowTree := buildTestTree(t, map[string]string{
		"file1.go": "package main\n\nfunc agent1() {}\nfunc agent2() {}\n",
	})

	// Head (final commit): user adds more to file2.go AFTER last checkpoint
	// file2.go has: 1 base line + 2 accumulated + 2 post-checkpoint = 5 lines total
	headTree := buildTestTree(t, map[string]string{
		"file1.go": "package main\n\nfunc agent1() {}\nfunc agent2() {}\n",
		"file2.go": "package util\n\n// User edit 1\n// User edit 2\n// User edit 3\n",
	})

	// filesTouched only includes file1.go (agent-touched)
	filesTouched := []string{"file1.go"}

	// PromptAttributions captured user edits to file2.go between checkpoints
	promptAttributions := []PromptAttribution{
		{
			CheckpointNumber: 1,
			UserLinesAdded:   2, // User edit 1, 2 (between checkpoints)
			UserLinesRemoved: 0,
			UserAddedPerFile: map[string]int{"file2.go": 2}, // Tracks which file the edits were in
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

// TestGetAllChangedFilesBetweenTrees tests the hash-based file change detection.
func TestGetAllChangedFilesBetweenTrees(t *testing.T) {
	storer := memory.NewStorage()

	// Helper to create a blob and return its hash
	//nolint:errcheck // Test helper - errors would cause test failures anyway
	createBlob := func(content string) plumbing.Hash {
		blob := storer.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		writer, _ := blob.Writer()
		_, _ = writer.Write([]byte(content))
		_ = writer.Close()
		hash, _ := storer.SetEncodedObject(blob)
		return hash
	}

	// Helper to create a tree from file entries
	//nolint:errcheck // Test helper - errors would cause test failures anyway
	createTree := func(files map[string]string) *object.Tree {
		var entries []object.TreeEntry
		for name, content := range files {
			entries = append(entries, object.TreeEntry{
				Name: name,
				Mode: 0o100644,
				Hash: createBlob(content),
			})
		}
		// Sort entries by name (git requirement)
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].Name < entries[j].Name
		})

		tree := &object.Tree{Entries: entries}
		treeObj := storer.NewEncodedObject()
		_ = tree.Encode(treeObj)
		hash, _ := storer.SetEncodedObject(treeObj)

		// Re-decode to get proper Tree object with hash
		decodedTree, _ := object.GetTree(storer, hash)
		return decodedTree
	}

	t.Run("both trees nil", func(t *testing.T) {
		result := getAllChangedFilesBetweenTrees(nil, nil)
		if result != nil {
			t.Errorf("expected nil, got %v", result)
		}
	})

	t.Run("tree1 nil (all files added)", func(t *testing.T) {
		tree2 := createTree(map[string]string{
			testFile1:  "content1",
			"file2.go": "content2",
		})

		result := getAllChangedFilesBetweenTrees(nil, tree2)
		sort.Strings(result)

		if len(result) != 2 {
			t.Fatalf("expected 2 changed files, got %d: %v", len(result), result)
		}
		if result[0] != testFile1 || result[1] != "file2.go" {
			t.Errorf("expected [file1.go, file2.go], got %v", result)
		}
	})

	t.Run("tree2 nil (all files deleted)", func(t *testing.T) {
		tree1 := createTree(map[string]string{
			testFile1: "content1",
		})

		result := getAllChangedFilesBetweenTrees(tree1, nil)

		if len(result) != 1 || result[0] != testFile1 {
			t.Errorf("expected [file1.go], got %v", result)
		}
	})

	t.Run("identical trees (no changes)", func(t *testing.T) {
		tree1 := createTree(map[string]string{
			testFile1:  "same content",
			"file2.go": "also same",
		})
		tree2 := createTree(map[string]string{
			testFile1:  "same content",
			"file2.go": "also same",
		})

		result := getAllChangedFilesBetweenTrees(tree1, tree2)

		if len(result) != 0 {
			t.Errorf("expected no changes, got %v", result)
		}
	})

	t.Run("one file modified", func(t *testing.T) {
		tree1 := createTree(map[string]string{
			testFile1:      "original",
			"unchanged.go": "stays same",
		})
		tree2 := createTree(map[string]string{
			testFile1:      "modified",
			"unchanged.go": "stays same",
		})

		result := getAllChangedFilesBetweenTrees(tree1, tree2)

		if len(result) != 1 || result[0] != testFile1 {
			t.Errorf("expected [file1.go], got %v", result)
		}
	})

	t.Run("file added and deleted", func(t *testing.T) {
		tree1 := createTree(map[string]string{
			"deleted.go": "will be removed",
			"stays.go":   "unchanged",
		})
		tree2 := createTree(map[string]string{
			"added.go": "new file",
			"stays.go": "unchanged",
		})

		result := getAllChangedFilesBetweenTrees(tree1, tree2)
		sort.Strings(result)

		if len(result) != 2 {
			t.Fatalf("expected 2 changed files, got %d: %v", len(result), result)
		}
		if result[0] != "added.go" || result[1] != "deleted.go" {
			t.Errorf("expected [added.go, deleted.go], got %v", result)
		}
	})
}

// TestEstimateUserSelfModifications tests the LIFO heuristic for user self-modifications.
func TestEstimateUserSelfModifications(t *testing.T) {
	tests := []struct {
		name                  string
		accumulatedUserAdded  map[string]int
		postCheckpointRemoved map[string]int
		expectedSelfModified  int
	}{
		{
			name:                  "no removals",
			accumulatedUserAdded:  map[string]int{"file.go": 5},
			postCheckpointRemoved: map[string]int{},
			expectedSelfModified:  0,
		},
		{
			name:                  "removals less than user added",
			accumulatedUserAdded:  map[string]int{"file.go": 5},
			postCheckpointRemoved: map[string]int{"file.go": 3},
			expectedSelfModified:  3, // All 3 removals are self-modifications
		},
		{
			name:                  "removals equal to user added",
			accumulatedUserAdded:  map[string]int{"file.go": 5},
			postCheckpointRemoved: map[string]int{"file.go": 5},
			expectedSelfModified:  5, // All 5 removals are self-modifications
		},
		{
			name:                  "removals exceed user added",
			accumulatedUserAdded:  map[string]int{"file.go": 3},
			postCheckpointRemoved: map[string]int{"file.go": 5},
			expectedSelfModified:  3, // Only 3 are self-modifications, 2 must be agent lines
		},
		{
			name:                  "no user additions to file",
			accumulatedUserAdded:  map[string]int{},
			postCheckpointRemoved: map[string]int{"file.go": 5},
			expectedSelfModified:  0, // All removals target agent lines
		},
		{
			name:                  "multiple files",
			accumulatedUserAdded:  map[string]int{"a.go": 3, "b.go": 2},
			postCheckpointRemoved: map[string]int{"a.go": 2, "b.go": 4},
			expectedSelfModified:  4, // 2 from a.go + 2 from b.go (capped at user additions)
		},
		{
			name:                  "removal from file user never touched",
			accumulatedUserAdded:  map[string]int{"a.go": 5},
			postCheckpointRemoved: map[string]int{"b.go": 3},
			expectedSelfModified:  0, // User never added to b.go, so all removals are agent lines
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := estimateUserSelfModifications(tt.accumulatedUserAdded, tt.postCheckpointRemoved)
			if result != tt.expectedSelfModified {
				t.Errorf("estimateUserSelfModifications() = %d, want %d", result, tt.expectedSelfModified)
			}
		})
	}
}

// TestCalculateAttributionWithAccumulated_UserSelfModification tests the per-file tracking fix:
// when a user modifies their own previously-added lines (not agent lines),
// it should NOT reduce the agent's contribution.
//
// Bug scenario before fix:
// 1. Agent adds 10 lines
// 2. User adds 5 lines of their own (captured in PromptAttribution)
// 3. User later removes 3 of their own lines and adds 3 different ones
// 4. OLD: humanModified=3 was subtracted from agent lines (WRONG)
// 5. NEW: humanModified=3 but userSelfModified=3, so agent lines unchanged (CORRECT)
func TestCalculateAttributionWithAccumulated_UserSelfModification(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow (checkpoint state): agent added 10 lines, user added 5 lines between checkpoints
	// The shadow includes both because it's a snapshot of the worktree at checkpoint time
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nagent9\nagent10\nuser1\nuser2\nuser3\nuser4\nuser5\n",
	})

	// Head (commit state): user removed 3 of their own lines and added 3 different ones
	// Agent lines are unchanged
	headTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nagent9\nagent10\nuser1\nuser2\nnew_user1\nnew_user2\nnew_user3\n",
	})

	filesTouched := []string{"main.go"}

	// PromptAttribution captured that user added 5 lines between checkpoints
	promptAttributions := []PromptAttribution{
		{
			CheckpointNumber: 2,
			UserLinesAdded:   5,
			UserLinesRemoved: 0,
			UserAddedPerFile: map[string]int{"main.go": 5}, // KEY: per-file tracking
		},
	}

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected calculation with per-file tracking:
	// - base → shadow: 15 lines added (10 agent + 5 user)
	// - accumulatedUserAdded: 5 (from PromptAttribution)
	// - totalAgentAdded: 15 - 5 = 10
	// - shadow → head: +3 lines added, -3 lines removed (user modification)
	// - totalUserAdded: 5 + 3 = 8
	// - totalUserRemoved: 3
	// - totalHumanModified: min(8, 3) = 3
	// - userSelfModified: min(3 removed from main.go, 5 user added to main.go) = 3
	// - humanModifiedAgent: 3 - 3 = 0 (no agent lines were modified!)
	// - agentLinesInCommit: 10 - 0 - 0 = 10 (CORRECT: agent lines unchanged)
	// - Total: 10 + 5 = 15
	// - Agent percentage: 10/15 = 66.7%

	t.Logf("Attribution: agent=%d, human_added=%d, human_modified=%d, total=%d, percentage=%.1f%%",
		result.AgentLines, result.HumanAdded, result.HumanModified, result.TotalCommitted, result.AgentPercentage)

	if result.AgentLines != 10 {
		t.Errorf("AgentLines = %d, want 10 (agent lines should NOT be reduced by user self-modifications)", result.AgentLines)
	}
	if result.HumanAdded != 5 {
		t.Errorf("HumanAdded = %d, want 5 (8 total - 3 modifications)", result.HumanAdded)
	}
	if result.HumanModified != 3 {
		t.Errorf("HumanModified = %d, want 3 (total modifications for reporting)", result.HumanModified)
	}
	if result.TotalCommitted != 15 {
		t.Errorf("TotalCommitted = %d, want 15", result.TotalCommitted)
	}
	if result.AgentPercentage < 66.6 || result.AgentPercentage > 66.8 {
		t.Errorf("AgentPercentage = %.1f%%, want ~66.7%%", result.AgentPercentage)
	}
}

// TestCalculateAttributionWithAccumulated_MixedModifications tests the case where
// user modifies both their own lines AND agent lines.
func TestCalculateAttributionWithAccumulated_MixedModifications(t *testing.T) {
	// Base: empty file
	baseTree := buildTestTree(t, map[string]string{
		"main.go": "",
	})

	// Shadow: agent added 10 lines, user added 3 lines
	shadowTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nagent9\nagent10\nuser1\nuser2\nuser3\n",
	})

	// Head: user removed 5 lines (3 own + 2 agent) and added 5 new lines
	// Net effect: user modified 5 lines total
	headTree := buildTestTree(t, map[string]string{
		"main.go": "agent1\nagent2\nagent3\nagent4\nagent5\nagent6\nagent7\nagent8\nnew1\nnew2\nnew3\nnew4\nnew5\n",
	})

	filesTouched := []string{"main.go"}

	promptAttributions := []PromptAttribution{
		{
			CheckpointNumber: 2,
			UserLinesAdded:   3,
			UserLinesRemoved: 0,
			UserAddedPerFile: map[string]int{"main.go": 3},
		},
	}

	result := CalculateAttributionWithAccumulated(
		baseTree, shadowTree, headTree, filesTouched, promptAttributions,
	)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Expected calculation:
	// - base → shadow: 13 lines added (10 agent + 3 user)
	// - accumulatedUserAdded: 3
	// - totalAgentAdded: 13 - 3 = 10
	// - shadow → head: +5 added, -5 removed
	// - totalUserAdded: 3 + 5 = 8
	// - totalUserRemoved: 5
	// - totalHumanModified: min(8, 5) = 5
	// - userSelfModified: min(5 removed, 3 user added) = 3 (user exhausted their pool)
	// - humanModifiedAgent: 5 - 3 = 2 (2 modifications targeted agent lines)
	// - agentLinesInCommit: 10 - 0 - 2 = 8 (reduced by modifications to agent lines only)
	// - pureUserAdded: 8 - 5 = 3
	// - Total: 10 + 3 = 13
	// - Agent percentage: 8/13 = 61.5%

	t.Logf("Attribution: agent=%d, human_added=%d, human_modified=%d, total=%d, percentage=%.1f%%",
		result.AgentLines, result.HumanAdded, result.HumanModified, result.TotalCommitted, result.AgentPercentage)

	if result.AgentLines != 8 {
		t.Errorf("AgentLines = %d, want 8 (10 - 2 modifications to agent lines)", result.AgentLines)
	}
	if result.HumanModified != 5 {
		t.Errorf("HumanModified = %d, want 5", result.HumanModified)
	}
	if result.TotalCommitted != 13 {
		t.Errorf("TotalCommitted = %d, want 13", result.TotalCommitted)
	}
	if result.AgentPercentage < 61.4 || result.AgentPercentage > 61.6 {
		t.Errorf("AgentPercentage = %.1f%%, want ~61.5%%", result.AgentPercentage)
	}
}

// TestCalculatePromptAttribution_PopulatesPerFile verifies that CalculatePromptAttribution
// correctly populates the UserAddedPerFile map.
func TestCalculatePromptAttribution_PopulatesPerFile(t *testing.T) {
	// Base: two files
	baseTree := buildTestTree(t, map[string]string{
		"a.go": "line1\n",
		"b.go": "line1\n",
	})

	// Last checkpoint: agent added lines to both files
	lastCheckpointTree := buildTestTree(t, map[string]string{
		"a.go": "line1\nagent1\n",
		"b.go": "line1\nagent1\nagent2\n",
	})

	// Current worktree: user added lines to both files
	worktreeFiles := map[string]string{
		"a.go": "line1\nagent1\nuser1\nuser2\nuser3\n", // +3 user lines
		"b.go": "line1\nagent1\nagent2\nuser1\n",       // +1 user line
	}

	result := CalculatePromptAttribution(baseTree, lastCheckpointTree, worktreeFiles, 2)

	if result.UserLinesAdded != 4 {
		t.Errorf("UserLinesAdded = %d, want 4 (3 + 1)", result.UserLinesAdded)
	}

	if result.UserAddedPerFile == nil {
		t.Fatal("UserAddedPerFile should not be nil")
	}

	if result.UserAddedPerFile["a.go"] != 3 {
		t.Errorf("UserAddedPerFile[a.go] = %d, want 3", result.UserAddedPerFile["a.go"])
	}
	if result.UserAddedPerFile["b.go"] != 1 {
		t.Errorf("UserAddedPerFile[b.go] = %d, want 1", result.UserAddedPerFile["b.go"])
	}
}
