package strategy

import (
	"strings"
	"time"

	"entire.io/cli/cmd/entire/cli/checkpoint"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// contains checks if a slice contains a string.
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// getAllChangedFilesBetweenTrees returns a list of all files that differ between two trees.
// This includes files that were added, modified, or deleted in either tree.
func getAllChangedFilesBetweenTrees(tree1, tree2 *object.Tree) []string {
	if tree1 == nil && tree2 == nil {
		return nil
	}

	fileSet := make(map[string]struct{})

	// Get all files from tree1
	if tree1 != nil {
		//nolint:errcheck // Errors ignored - just collecting file names for diff comparison
		_ = tree1.Files().ForEach(func(f *object.File) error {
			fileSet[f.Name] = struct{}{}
			return nil
		})
	}

	// Get all files from tree2
	if tree2 != nil {
		//nolint:errcheck // Errors ignored - just collecting file names for diff comparison
		_ = tree2.Files().ForEach(func(f *object.File) error {
			fileSet[f.Name] = struct{}{}
			return nil
		})
	}

	// Convert set to slice and filter to only files that actually changed
	var changed []string
	for filePath := range fileSet {
		content1 := getFileContent(tree1, filePath)
		content2 := getFileContent(tree2, filePath)
		if content1 != content2 {
			changed = append(changed, filePath)
		}
	}

	return changed
}

// getFileContent retrieves the content of a file from a tree.
// Returns empty string if the file doesn't exist, can't be read, or is a binary file.
//
// Binary files (files containing null bytes) are silently excluded from attribution
// calculations because line-based diffing doesn't apply to binary content. This means
// binary files (images, compiled binaries, etc.) won't appear in attribution metrics
// even if they were added or modified. This is intentional - attribution measures code
// contributions via line counting, which only makes sense for text files.
//
// TODO: Consider tracking binary file counts separately (e.g., BinaryFilesChanged field)
// to provide visibility into non-text file modifications.
func getFileContent(tree *object.Tree, path string) string {
	if tree == nil {
		return ""
	}

	file, err := tree.File(path)
	if err != nil {
		return ""
	}

	content, err := file.Contents()
	if err != nil {
		return ""
	}

	// Skip binary files (contain null bytes).
	// Binary files are excluded from line-based attribution calculations.
	// This is intentional - line counting only applies to text files.
	if strings.Contains(content, "\x00") {
		return ""
	}

	return content
}

// diffLines compares two strings and returns line-level diff stats.
// Returns (unchanged, added, removed) line counts.
func diffLines(checkpointContent, committedContent string) (unchanged, added, removed int) {
	// Handle edge cases
	if checkpointContent == committedContent {
		return countLinesStr(committedContent), 0, 0
	}
	if checkpointContent == "" {
		return 0, countLinesStr(committedContent), 0
	}
	if committedContent == "" {
		return 0, 0, countLinesStr(checkpointContent)
	}

	dmp := diffmatchpatch.New()

	// Convert to line-based diff using DiffLinesToChars/DiffCharsToLines pattern
	text1, text2, lineArray := dmp.DiffLinesToChars(checkpointContent, committedContent)
	diffs := dmp.DiffMain(text1, text2, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)

	for _, d := range diffs {
		lines := countLinesStr(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			unchanged += lines
		case diffmatchpatch.DiffInsert:
			added += lines
		case diffmatchpatch.DiffDelete:
			removed += lines
		}
	}

	return unchanged, added, removed
}

// countLinesStr returns the number of lines in a string.
// An empty string has 0 lines. A string without newlines has 1 line.
// This is used for both file content and diff text segments.
func countLinesStr(content string) int {
	if content == "" {
		return 0
	}
	lines := strings.Count(content, "\n")
	// If content doesn't end with newline, add 1 for the last line
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}

// CalculateAttributionWithAccumulated computes final attribution using accumulated prompt data.
// This provides more accurate attribution than tree-only comparison because it captures
// user edits that happened between checkpoints (which would otherwise be mixed into the
// checkpoint snapshots).
//
// The calculation:
// 1. Sum user edits from PromptAttributions (captured at each prompt start)
// 2. Add user edits after the final checkpoint (shadow → head diff)
// 3. Calculate agent lines from base → shadow
// 4. Compute percentages
//
// Note: Binary files (detected by null bytes) are silently excluded from attribution
// calculations since line-based diffing only applies to text files.
func CalculateAttributionWithAccumulated(
	baseTree *object.Tree,
	shadowTree *object.Tree,
	headTree *object.Tree,
	filesTouched []string,
	promptAttributions []PromptAttribution,
) *checkpoint.InitialAttribution {
	if len(filesTouched) == 0 {
		return nil
	}

	// Sum accumulated user lines from prompt attributions
	var accumulatedUserAdded, accumulatedUserRemoved int
	for _, pa := range promptAttributions {
		accumulatedUserAdded += pa.UserLinesAdded
		accumulatedUserRemoved += pa.UserLinesRemoved
	}

	// Calculate attribution for agent-touched files
	// IMPORTANT: shadowTree is a snapshot of the worktree at checkpoint time,
	// which includes both agent work AND accumulated user edits (to agent-touched files).
	// So base→shadow diff = (agent work + accumulated user work to these files).
	var totalAgentAndUserWork int
	var postCheckpointUserAdded, postCheckpointUserRemoved int

	for _, filePath := range filesTouched {
		baseContent := getFileContent(baseTree, filePath)
		shadowContent := getFileContent(shadowTree, filePath)
		headContent := getFileContent(headTree, filePath)

		// Total work in shadow: base → shadow (agent + accumulated user work for this file)
		_, workAdded, _ := diffLines(baseContent, shadowContent)
		totalAgentAndUserWork += workAdded

		// Post-checkpoint user edits: shadow → head (only post-checkpoint edits for this file)
		_, postUserAdded, postUserRemoved := diffLines(shadowContent, headContent)
		postCheckpointUserAdded += postUserAdded
		postCheckpointUserRemoved += postUserRemoved
	}

	// Calculate total user edits to non-agent files (files not in filesTouched)
	// These files are not in the shadow tree, so base→head captures ALL their user edits
	nonAgentFiles := getAllChangedFilesBetweenTrees(baseTree, headTree)
	var allUserEditsToNonAgentFiles int
	for _, filePath := range nonAgentFiles {
		if contains(filesTouched, filePath) {
			continue // Skip agent-touched files
		}

		baseContent := getFileContent(baseTree, filePath)
		headContent := getFileContent(headTree, filePath)
		_, userAdded, _ := diffLines(baseContent, headContent)
		allUserEditsToNonAgentFiles += userAdded
	}

	// Separate accumulated edits by file type
	// accumulatedUserAdded includes edits to BOTH agent and non-agent files
	// For agent work calculation, we only need to subtract edits to agent files
	// Heuristic: assume accumulated edits to non-agent files = min(total edits to non-agent files, total accumulated)
	accumulatedToNonAgentFiles := min(allUserEditsToNonAgentFiles, accumulatedUserAdded)
	accumulatedToAgentFiles := accumulatedUserAdded - accumulatedToNonAgentFiles

	// Agent work = (base→shadow for agent files) - (accumulated user edits to agent files only)
	totalAgentAdded := max(0, totalAgentAndUserWork-accumulatedToAgentFiles)

	// Post-checkpoint edits to non-agent files = total edits - accumulated portion
	postToNonAgentFiles := allUserEditsToNonAgentFiles - accumulatedToNonAgentFiles

	// Total user contribution = accumulated (all files) + post-checkpoint (agent files) + post-checkpoint (non-agent files)
	totalUserAdded := accumulatedUserAdded + postCheckpointUserAdded + postToNonAgentFiles
	totalUserRemoved := accumulatedUserRemoved + postCheckpointUserRemoved

	// Estimate modified lines (user changed existing agent lines)
	// Lines that were both added and removed are treated as modifications.
	humanModified := min(totalUserAdded, totalUserRemoved)
	pureUserAdded := totalUserAdded - humanModified
	pureUserRemoved := totalUserRemoved - humanModified

	// Total net additions = agent additions + pure user additions - pure user removals
	// This reconstructs the base → head diff from our tracked changes.
	// Note: This measures "net new lines added to the codebase" not total file size.
	// pureUserRemoved represents agent lines that the user deleted, so we subtract them.
	totalCommitted := totalAgentAdded + pureUserAdded - pureUserRemoved
	if totalCommitted <= 0 {
		// Fallback for delete-only commits or when removals exceed additions
		// Note: If both are 0 (deletion-only commit where agent added nothing),
		// totalCommitted will be 0 and percentage will be 0. This is expected -
		// the attribution percentage is only meaningful for commits that add code.
		totalCommitted = max(0, totalAgentAdded)
	}

	// Calculate agent lines actually in the commit (excluding removed and modified)
	// Agent added lines, but user removed some and modified others.
	// Modified lines are now attributed to the user, not the agent.
	// Clamp to 0 to handle cases where user removed/modified more than agent added.
	agentLinesInCommit := max(0, totalAgentAdded-pureUserRemoved-humanModified)

	// Calculate percentage
	var agentPercentage float64
	if totalCommitted > 0 {
		agentPercentage = float64(agentLinesInCommit) / float64(totalCommitted) * 100
	}

	return &checkpoint.InitialAttribution{
		CalculatedAt:    time.Now(),
		AgentLines:      agentLinesInCommit,
		HumanAdded:      pureUserAdded,
		HumanModified:   humanModified,
		HumanRemoved:    pureUserRemoved,
		TotalCommitted:  totalCommitted,
		AgentPercentage: agentPercentage,
	}
}

// CalculatePromptAttribution computes line-level attribution at the start of a prompt.
// This captures user edits since the last checkpoint BEFORE the agent makes changes.
//
// Parameters:
//   - baseTree: the tree at session start (the base commit)
//   - lastCheckpointTree: the tree from the previous checkpoint (nil if first checkpoint)
//   - worktreeFiles: map of file path → current worktree content for files that changed
//   - checkpointNumber: which checkpoint we're about to create (1-indexed)
//
// Returns the attribution data to store in session state. For checkpoint 1 (when
// lastCheckpointTree is nil), AgentLinesAdded/Removed will be 0 since there's no
// previous checkpoint to measure cumulative agent work against.
//
// Note: Binary files (detected by null bytes) in reference trees are silently excluded
// from attribution calculations since line-based diffing only applies to text files.
func CalculatePromptAttribution(
	baseTree *object.Tree,
	lastCheckpointTree *object.Tree,
	worktreeFiles map[string]string,
	checkpointNumber int,
) PromptAttribution {
	result := PromptAttribution{
		CheckpointNumber: checkpointNumber,
	}

	if len(worktreeFiles) == 0 {
		return result
	}

	// Determine reference tree for user changes (last checkpoint or base)
	referenceTree := lastCheckpointTree
	if referenceTree == nil {
		referenceTree = baseTree
	}

	for filePath, worktreeContent := range worktreeFiles {
		referenceContent := getFileContent(referenceTree, filePath)
		baseContent := getFileContent(baseTree, filePath)

		// User changes: diff(reference, worktree)
		// These are changes since the last checkpoint that the agent didn't make
		_, userAdded, userRemoved := diffLines(referenceContent, worktreeContent)
		result.UserLinesAdded += userAdded
		result.UserLinesRemoved += userRemoved

		// Agent lines so far: diff(base, lastCheckpoint)
		// Only calculate if we have a previous checkpoint
		if lastCheckpointTree != nil {
			checkpointContent := getFileContent(lastCheckpointTree, filePath)
			_, agentAdded, agentRemoved := diffLines(baseContent, checkpointContent)
			result.AgentLinesAdded += agentAdded
			result.AgentLinesRemoved += agentRemoved
		}
	}

	return result
}
