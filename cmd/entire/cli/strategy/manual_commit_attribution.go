package strategy

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/gitops"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// getAllChangedFiles returns all files that changed between the attribution base
// and HEAD. When commit hashes and repoDir are provided, uses fast git diff-tree CLI;
// otherwise falls back to go-git tree walk (used by CondenseSessionByID / doctor command).
func getAllChangedFiles(ctx context.Context, baseTree, headTree *object.Tree, repoDir, baseCommitHash, headCommitHash string) ([]string, error) {
	// Fast path: use git diff-tree when commit hashes are available
	if baseCommitHash != "" && headCommitHash != "" {
		return gitops.DiffTreeFileList(ctx, repoDir, baseCommitHash, headCommitHash) //nolint:wrapcheck // Propagating gitops error
	}

	// Slow path: go-git tree walk (CondenseSessionByID fallback)
	return getAllChangedFilesBetweenTreesSlow(ctx, baseTree, headTree)
}

// getAllChangedFilesBetweenTreesSlow returns a list of all files that differ between two trees.
// This is the slow fallback path using go-git tree walks, used only when commit hashes
// are not available (e.g., CondenseSessionByID / doctor command).
func getAllChangedFilesBetweenTreesSlow(ctx context.Context, tree1, tree2 *object.Tree) ([]string, error) {
	if tree1 == nil && tree2 == nil {
		return nil, nil
	}

	tree1Hashes := make(map[string]string)
	tree2Hashes := make(map[string]string)

	if tree1 != nil {
		if err := tree1.Files().ForEach(func(f *object.File) error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // Propagating context cancellation
			}
			tree1Hashes[f.Name] = f.Hash.String()
			return nil
		}); err != nil {
			return nil, err //nolint:wrapcheck // Propagating context/iteration error
		}
	}

	if tree2 != nil {
		if err := tree2.Files().ForEach(func(f *object.File) error {
			if err := ctx.Err(); err != nil {
				return err //nolint:wrapcheck // Propagating context cancellation
			}
			tree2Hashes[f.Name] = f.Hash.String()
			return nil
		}); err != nil {
			return nil, err //nolint:wrapcheck // Propagating context/iteration error
		}
	}

	var changed []string

	for path, hash1 := range tree1Hashes {
		if hash2, exists := tree2Hashes[path]; !exists || hash1 != hash2 {
			changed = append(changed, path)
		}
	}

	for path := range tree2Hashes {
		if _, exists := tree1Hashes[path]; !exists {
			changed = append(changed, path)
		}
	}

	return changed, nil
}

// getFileContent retrieves the content of a file from a tree.
// Returns empty string if the file doesn't exist, can't be read, or is a binary file.
//
// Binary files are silently excluded from attribution calculations because line-based
// diffing doesn't apply to binary content. This means binary files (images, compiled
// binaries, etc.) won't appear in attribution metrics even if they were added or modified.
// This is intentional - attribution measures code contributions via line counting,
// which only makes sense for text files.
//
// Uses go-git's IsBinary() which implements git's binary detection algorithm.
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

	// Use git's binary detection algorithm
	isBinary, err := file.IsBinary()
	if err != nil || isBinary {
		return ""
	}

	content, err := file.Contents()
	if err != nil {
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

// AttributionParams bundles the inputs for CalculateAttributionWithAccumulated.
type AttributionParams struct {
	BaseTree              *object.Tree        // Session base commit tree
	ShadowTree            *object.Tree        // Shadow branch tree (checkpoint snapshot)
	HeadTree              *object.Tree        // HEAD commit tree
	ParentTree            *object.Tree        // HEAD's first parent tree (nil for initial commits)
	FilesTouched          []string            // Agent-touched file paths
	PromptAttributions    []PromptAttribution // Per-prompt user edit snapshots
	RepoDir               string              // Worktree path for git CLI commands
	ParentCommitHash      string              // HEAD's first parent hash (preferred diff base for non-agent files)
	AttributionBaseCommit string              // Session base commit hash (fallback for non-agent file detection)
	HeadCommitHash        string              // HEAD commit hash for git diff-tree
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
// 4. Estimate user self-modifications vs agent modifications using per-file tracking
// 5. Compute percentages
//
// ParentCommitHash→HeadCommitHash is preferred for non-agent file detection so only files
// from THIS commit count. For initial commits (no parent), falls back to
// AttributionBaseCommit→HeadCommitHash. When hashes are empty, falls back to go-git tree walk.
//
// Note: Binary files (detected by null bytes) are silently excluded from attribution
// calculations since line-based diffing only applies to text files.
//
// See docs/architecture/attribution.md for details on the per-file tracking approach.
func CalculateAttributionWithAccumulated(ctx context.Context, p AttributionParams) *checkpoint.InitialAttribution {
	if len(p.FilesTouched) == 0 {
		return nil
	}

	// Phase 1: Accumulate user edits from prompt attributions
	accum := accumulatePromptEdits(p.PromptAttributions)

	// Phase 2: Diff agent-touched files (base→shadow and shadow→head)
	agentDiffs := diffAgentTouchedFiles(p.BaseTree, p.ShadowTree, p.HeadTree, p.FilesTouched)

	// Phase 3: Enumerate and diff non-agent files
	nonAgent, err := diffNonAgentFiles(ctx, p)
	if err != nil {
		return nil
	}

	// Phase 4: Classify accumulated edits as agent vs non-agent
	classified := classifyAccumulatedEdits(accum, p.FilesTouched, nonAgent.committedNonAgentSet)

	// Phase 5: Compute derived metrics
	totalAgentAdded := max(0, agentDiffs.totalAgentAndUserWorkAdded-classified.toAgentFiles)
	postToNonAgentFiles := max(0, nonAgent.userEditsToNonAgentFiles-classified.toCommittedNonAgentFiles)

	relevantAccumulatedUser := classified.toAgentFiles + classified.toCommittedNonAgentFiles
	totalUserAdded := relevantAccumulatedUser + agentDiffs.postCheckpointUserAdded + postToNonAgentFiles
	// Use per-file filtered removals (symmetric with totalUserAdded) to avoid
	// double-counting non-agent removals that also appear in nonAgent.userRemovedFromNonAgentFiles.
	relevantAccumulatedRemoved := classified.removedFromAgentFiles + classified.removedFromCommittedNonAgent
	totalUserRemoved := relevantAccumulatedRemoved + agentDiffs.postCheckpointUserRemoved

	totalHumanModified := min(totalUserAdded, totalUserRemoved)
	userSelfModified := estimateUserSelfModifications(accum.addedPerFile, agentDiffs.postCheckpointUserRemovedPerFile)
	humanModifiedAgent := max(0, totalHumanModified-userSelfModified)

	pureUserAdded := totalUserAdded - totalHumanModified
	pureUserRemoved := totalUserRemoved - totalHumanModified

	totalCommitted := totalAgentAdded + pureUserAdded - pureUserRemoved
	if totalCommitted <= 0 {
		totalCommitted = max(0, totalAgentAdded)
	}

	agentLinesInCommit := max(0, totalAgentAdded-pureUserRemoved-humanModifiedAgent)

	// Phase 6: Compute agent deletions and non-agent removals
	agentRemovedInCommit := computeAgentDeletions(p.BaseTree, p.ShadowTree, p.HeadTree, p.FilesTouched, classified.removedFromAgentFiles)

	agentChangedLines := agentLinesInCommit + agentRemovedInCommit
	totalLinesChanged := agentChangedLines + pureUserAdded + totalHumanModified + pureUserRemoved + nonAgent.userRemovedFromNonAgentFiles

	var agentPercentage float64
	if totalLinesChanged > 0 {
		agentPercentage = float64(agentChangedLines) / float64(totalLinesChanged) * 100
	}

	return &checkpoint.InitialAttribution{
		CalculatedAt:      time.Now().UTC(),
		AgentLines:        agentLinesInCommit,
		AgentRemoved:      agentRemovedInCommit,
		HumanAdded:        pureUserAdded,
		HumanModified:     totalHumanModified,
		HumanRemoved:      pureUserRemoved,
		TotalCommitted:    totalCommitted,
		TotalLinesChanged: totalLinesChanged,
		AgentPercentage:   agentPercentage,
		MetricVersion:     2, // changed-lines % (adds + removes), distinct from legacy additions-only %
	}
}

// accumulatedEdits holds aggregated user edit data from prompt attributions.
type accumulatedEdits struct {
	userAdded      int
	userRemoved    int
	addedPerFile   map[string]int
	removedPerFile map[string]int
}

// accumulatePromptEdits sums user additions and removals from all prompt attributions.
func accumulatePromptEdits(promptAttributions []PromptAttribution) accumulatedEdits {
	result := accumulatedEdits{
		addedPerFile:   make(map[string]int),
		removedPerFile: make(map[string]int),
	}
	for _, pa := range promptAttributions {
		result.userAdded += pa.UserLinesAdded
		result.userRemoved += pa.UserLinesRemoved
		for filePath, added := range pa.UserAddedPerFile {
			result.addedPerFile[filePath] += added
		}
		for filePath, removed := range pa.UserRemovedPerFile {
			result.removedPerFile[filePath] += removed
		}
	}
	return result
}

// agentFileDiffs holds diff results for agent-touched files.
type agentFileDiffs struct {
	totalAgentAndUserWorkAdded       int
	postCheckpointUserAdded          int
	postCheckpointUserRemoved        int
	postCheckpointUserRemovedPerFile map[string]int
}

// diffAgentTouchedFiles computes base→shadow and shadow→head diffs for agent files.
// shadowTree is a snapshot at checkpoint time containing both agent work AND accumulated
// user edits, so base→shadow = (agent work + accumulated user work to these files).
func diffAgentTouchedFiles(baseTree, shadowTree, headTree *object.Tree, filesTouched []string) agentFileDiffs {
	result := agentFileDiffs{
		postCheckpointUserRemovedPerFile: make(map[string]int),
	}
	for _, filePath := range filesTouched {
		baseContent := getFileContent(baseTree, filePath)
		shadowContent := getFileContent(shadowTree, filePath)
		headContent := getFileContent(headTree, filePath)

		_, workAdded, _ := diffLines(baseContent, shadowContent)
		result.totalAgentAndUserWorkAdded += workAdded

		_, postUserAdded, postUserRemoved := diffLines(shadowContent, headContent)
		result.postCheckpointUserAdded += postUserAdded
		result.postCheckpointUserRemoved += postUserRemoved

		if postUserRemoved > 0 {
			result.postCheckpointUserRemovedPerFile[filePath] = postUserRemoved
		}
	}
	return result
}

// nonAgentFileDiffs holds diff results for files not touched by the agent.
type nonAgentFileDiffs struct {
	allChangedFiles              []string
	committedNonAgentSet         map[string]struct{}
	userEditsToNonAgentFiles     int
	userRemovedFromNonAgentFiles int
}

// diffNonAgentFiles enumerates files changed in the commit that weren't touched by the agent,
// and computes their user additions and removals.
// Prefers parentCommitHash→headCommitHash so only THIS commit's files count.
func diffNonAgentFiles(ctx context.Context, p AttributionParams) (nonAgentFileDiffs, error) {
	diffBaseCommit := p.ParentCommitHash
	if diffBaseCommit == "" {
		diffBaseCommit = p.AttributionBaseCommit
	}
	allChangedFiles, err := getAllChangedFiles(ctx, p.BaseTree, p.HeadTree, p.RepoDir, diffBaseCommit, p.HeadCommitHash)
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "attribution"),
			"attribution: failed to enumerate changed files",
			slog.String("error", err.Error()),
		)
		return nonAgentFileDiffs{}, err
	}

	// Use parentTree for line counting when available so only THIS commit's
	// changes are counted. For initial commits, fall back to baseTree.
	nonAgentDiffTree := p.ParentTree
	if nonAgentDiffTree == nil {
		nonAgentDiffTree = p.BaseTree
	}

	result := nonAgentFileDiffs{
		allChangedFiles:      allChangedFiles,
		committedNonAgentSet: make(map[string]struct{}, len(allChangedFiles)),
	}
	for _, filePath := range allChangedFiles {
		if slices.Contains(p.FilesTouched, filePath) {
			continue
		}
		result.committedNonAgentSet[filePath] = struct{}{}

		diffBaseContent := getFileContent(nonAgentDiffTree, filePath)
		headContent := getFileContent(p.HeadTree, filePath)
		_, userAdded, userRemoved := diffLines(diffBaseContent, headContent)
		result.userEditsToNonAgentFiles += userAdded
		result.userRemovedFromNonAgentFiles += userRemoved
	}
	return result, nil
}

// classifiedEdits holds accumulated user edits split by file category.
type classifiedEdits struct {
	toAgentFiles                 int
	toCommittedNonAgentFiles     int
	removedFromAgentFiles        int
	removedFromCommittedNonAgent int
}

// classifyAccumulatedEdits separates accumulated user edits into agent-file vs non-agent-file
// buckets. Only files actually committed are counted — worktree-only changes are excluded.
func classifyAccumulatedEdits(accum accumulatedEdits, filesTouched []string, committedNonAgentSet map[string]struct{}) classifiedEdits {
	var result classifiedEdits
	for filePath, added := range accum.addedPerFile {
		if slices.Contains(filesTouched, filePath) {
			result.toAgentFiles += added
		} else if _, ok := committedNonAgentSet[filePath]; ok {
			result.toCommittedNonAgentFiles += added
		}
	}
	for filePath, removed := range accum.removedPerFile {
		if slices.Contains(filesTouched, filePath) {
			result.removedFromAgentFiles += removed
		} else if _, ok := committedNonAgentSet[filePath]; ok {
			result.removedFromCommittedNonAgent += removed
		}
	}
	return result
}

// computeAgentDeletions calculates agent-removed lines that actually remain deleted in the commit.
// Per-file: takes min(base→shadow removed, base→head removed) to avoid over-reporting when
// the user re-adds lines the agent deleted. Subtracts accumulated user removals to agent files.
func computeAgentDeletions(baseTree, shadowTree, headTree *object.Tree, filesTouched []string, accumulatedRemovedToAgentFiles int) int {
	var agentRemovedInCommit int
	for _, filePath := range filesTouched {
		baseContent := getFileContent(baseTree, filePath)
		shadowContent := getFileContent(shadowTree, filePath)
		headContent := getFileContent(headTree, filePath)

		_, _, removedBaseToShadow := diffLines(baseContent, shadowContent)
		_, _, removedBaseToHead := diffLines(baseContent, headContent)

		agentRemovedInCommit += min(removedBaseToShadow, removedBaseToHead)
	}
	return max(0, agentRemovedInCommit-accumulatedRemovedToAgentFiles)
}

// estimateUserSelfModifications estimates how many removed lines were the user's own additions.
// Uses LIFO assumption: when a user removes lines from a file, they likely remove their own
// recent additions before touching agent lines.
//
// See docs/architecture/attribution.md for the rationale behind this heuristic.
func estimateUserSelfModifications(
	accumulatedUserAddedPerFile map[string]int,
	postCheckpointUserRemovedPerFile map[string]int,
) int {
	var selfModified int
	for filePath, removed := range postCheckpointUserRemovedPerFile {
		userAddedToFile := accumulatedUserAddedPerFile[filePath]
		// User can only self-modify up to what they previously added
		selfModified += min(removed, userAddedToFile)
	}
	return selfModified
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
		CheckpointNumber:   checkpointNumber,
		UserAddedPerFile:   make(map[string]int),
		UserRemovedPerFile: make(map[string]int),
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

		// Track per-file user additions and removals for accurate attribution.
		// Additions: distinguishing user self-modifications from agent modifications.
		// Removals: subtracting only agent-file removals from agent deletion credit.
		if userAdded > 0 {
			result.UserAddedPerFile[filePath] = userAdded
		}
		if userRemoved > 0 {
			result.UserRemovedPerFile[filePath] = userRemoved
		}

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
