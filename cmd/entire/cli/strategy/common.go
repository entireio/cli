package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Common branch name constants for default branch detection.
const (
	branchMain   = "main"
	branchMaster = "master"
)

// errStop is a sentinel error used to break out of git log iteration.
// Shared across strategies that iterate through git commits.
// NOTE: A similar sentinel exists in checkpoint/temporary.go - this is intentional.
// Each package needs its own package-scoped sentinel for git log iteration patterns.
var errStop = errors.New("stop iteration")

// IsAncestorOf checks if commit is an ancestor of (or equal to) target.
// Returns true if target can reach commit by following parent links.
// Limits search to 1000 commits to avoid excessive traversal.
func IsAncestorOf(repo *git.Repository, commit, target plumbing.Hash) bool {
	if commit == target {
		return true
	}

	iter, err := repo.Log(&git.LogOptions{From: target})
	if err != nil {
		return false
	}
	defer iter.Close()

	found := false
	count := 0
	_ = iter.ForEach(func(c *object.Commit) error { //nolint:errcheck // Best-effort search, errors are non-fatal
		count++
		if count > 1000 {
			return errStop
		}
		if c.Hash == commit {
			found = true
			return errStop
		}
		return nil
	})

	return found
}

// ListCheckpoints returns all checkpoints from the entire/checkpoints/v1 branch.
// Scans sharded paths: <id[:2]>/<id[2:]>/ directories containing metadata.json.
// Used by both manual-commit and auto-commit strategies.
func ListCheckpoints() ([]CheckpointInfo, error) {
	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open git repository: %w", err)
	}

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		//nolint:nilerr // No sessions branch yet is expected, return empty list
		return []CheckpointInfo{}, nil
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get commit object: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get commit tree: %w", err)
	}

	var checkpoints []CheckpointInfo

	// Scan sharded structure: <2-char-prefix>/<remaining-id>/metadata.json
	// The tree has 2-character directories (hex buckets)
	for _, bucketEntry := range tree.Entries {
		if bucketEntry.Mode != filemode.Dir {
			continue
		}
		// Bucket should be 2 hex chars
		if len(bucketEntry.Name) != 2 {
			continue
		}

		bucketTree, treeErr := repo.TreeObject(bucketEntry.Hash)
		if treeErr != nil {
			continue
		}

		// Each entry in the bucket is the remaining part of the checkpoint ID
		for _, checkpointEntry := range bucketTree.Entries {
			if checkpointEntry.Mode != filemode.Dir {
				continue
			}

			checkpointTree, cpTreeErr := repo.TreeObject(checkpointEntry.Hash)
			if cpTreeErr != nil {
				continue
			}

			// Reconstruct checkpoint ID: <bucket><remaining>
			checkpointIDStr := bucketEntry.Name + checkpointEntry.Name
			checkpointID, cpErr := id.NewCheckpointID(checkpointIDStr)
			if cpErr != nil {
				// Skip invalid checkpoint IDs
				continue
			}

			info := CheckpointInfo{
				CheckpointID: checkpointID,
			}

			// Get details from metadata file (CheckpointSummary format)
			if metadataFile, fileErr := checkpointTree.File(paths.MetadataFileName); fileErr == nil {
				if content, contentErr := metadataFile.Contents(); contentErr == nil {
					var summary checkpoint.CheckpointSummary
					if json.Unmarshal([]byte(content), &summary) == nil && len(summary.Sessions) > 0 {
						info.CheckpointsCount = summary.CheckpointsCount
						info.FilesTouched = summary.FilesTouched
						info.SessionCount = len(summary.Sessions)

						// Read session-level metadata for Agent, SessionID, CreatedAt, SessionIDs
						for i, sessionPaths := range summary.Sessions {
							if sessionPaths.Metadata != "" {
								// SessionFilePaths now contains absolute paths with leading "/"
								// Strip the leading "/" for tree.File() which expects paths without leading slash
								sessionMetadataPath := strings.TrimPrefix(sessionPaths.Metadata, "/")
								if sessionFile, sErr := tree.File(sessionMetadataPath); sErr == nil {
									if sessionContent, scErr := sessionFile.Contents(); scErr == nil {
										var sessionMetadata checkpoint.CommittedMetadata
										if json.Unmarshal([]byte(sessionContent), &sessionMetadata) == nil {
											info.SessionIDs = append(info.SessionIDs, sessionMetadata.SessionID)
											// Use first session's metadata for Agent, SessionID, CreatedAt
											if i == 0 {
												info.Agent = sessionMetadata.Agent
												info.SessionID = sessionMetadata.SessionID
												info.CreatedAt = sessionMetadata.CreatedAt
												info.IsTask = sessionMetadata.IsTask
												info.ToolUseID = sessionMetadata.ToolUseID
											}
										}
									}
								}
							}
						}
					}
				}
			}

			checkpoints = append(checkpoints, info)
		}
	}

	// Sort by time (most recent first)
	sort.Slice(checkpoints, func(i, j int) bool {
		return checkpoints[i].CreatedAt.After(checkpoints[j].CreatedAt)
	})

	return checkpoints, nil
}

const (
	entireGitignore    = ".entire/.gitignore"
	entireDir          = ".entire"
	gitDir             = ".git"
	claudeDir          = ".claude"
	shadowBranchPrefix = "entire/"

	// DefaultAgentType is the generic fallback agent type name
	DefaultAgentType = agent.AgentTypeUnknown
)

// isSpecificAgentType returns true if the agent type is a known, specific value
// (not empty and not the generic "Agent" fallback).
func isSpecificAgentType(t agent.AgentType) bool {
	return t != "" && t != DefaultAgentType
}

// resolveAgentType picks the best agent type from the context and existing state.
// Priority: existing state (if specific) > context value > default fallback.
func resolveAgentType(ctxAgentType agent.AgentType, state *SessionState) agent.AgentType {
	if state != nil && isSpecificAgentType(state.AgentType) {
		return state.AgentType
	}
	if ctxAgentType != "" {
		return ctxAgentType
	}
	return DefaultAgentType
}

// ensureMetadataBranch creates the orphan entire/checkpoints/v1 branch if it doesn't exist.
// This branch has no parent and starts with an empty tree.
func EnsureMetadataBranch(repo *git.Repository) error {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)

	// Check if branch already exists
	_, err := repo.Reference(refName, true)
	if err == nil {
		// Branch already exists
		return nil
	}

	// Create empty tree (no files)
	emptyTree := &object.Tree{Entries: []object.TreeEntry{}}
	obj := repo.Storer.NewEncodedObject()
	if err := emptyTree.Encode(obj); err != nil {
		return fmt.Errorf("failed to encode empty tree: %w", err)
	}
	emptyTreeHash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return fmt.Errorf("failed to store empty tree: %w", err)
	}

	// Create orphan commit (no parent)
	now := time.Now()
	sig := object.Signature{
		Name:  "Entire",
		Email: "entire@local",
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:  emptyTreeHash,
		Author:    sig,
		Committer: sig,
		Message:   "Initialize metadata branch\n\nThis branch stores session metadata for the auto-commit strategy.\n",
	}
	// Note: No ParentHashes - this is an orphan commit

	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return fmt.Errorf("failed to encode orphan commit: %w", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		return fmt.Errorf("failed to store orphan commit: %w", err)
	}

	// Create branch reference
	ref := plumbing.NewHashReference(refName, commitHash)
	if err := repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to create metadata branch: %w", err)
	}

	fmt.Fprintf(os.Stderr, "✓ Created orphan branch '%s' for session metadata\n", paths.MetadataBranchName)
	return nil
}

// readCheckpointMetadata reads metadata.json from a checkpoint path on entire/checkpoints/v1.
// With the new format, root metadata.json is a CheckpointSummary with Agents array.
// This function reads the summary and extracts relevant fields into CheckpointInfo,
// also reading session-level metadata for IsTask/ToolUseID fields.
func ReadCheckpointMetadata(tree *object.Tree, checkpointPath string) (*CheckpointInfo, error) {
	metadataPath := checkpointPath + "/metadata.json"
	file, err := tree.File(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find metadata at %s: %w", metadataPath, err)
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata: %w", err)
	}

	// Try to parse as CheckpointSummary first (new format)
	var summary checkpoint.CheckpointSummary
	if err := json.Unmarshal([]byte(content), &summary); err == nil {
		// If we have sessions array, this is the new format
		if len(summary.Sessions) > 0 {
			info := &CheckpointInfo{
				CheckpointID:     summary.CheckpointID,
				CheckpointsCount: summary.CheckpointsCount,
				FilesTouched:     summary.FilesTouched,
				SessionCount:     len(summary.Sessions),
			}

			// Read all sessions' metadata to populate SessionIDs and get other fields from first session
			var sessionIDs []string
			for i, sessionPaths := range summary.Sessions {
				if sessionPaths.Metadata != "" {
					// SessionFilePaths now contains absolute paths with leading "/"
					// Strip the leading "/" for tree.File() which expects paths without leading slash
					sessionMetadataPath := strings.TrimPrefix(sessionPaths.Metadata, "/")
					if sessionFile, err := tree.File(sessionMetadataPath); err == nil {
						if sessionContent, err := sessionFile.Contents(); err == nil {
							var sessionMetadata checkpoint.CommittedMetadata
							if json.Unmarshal([]byte(sessionContent), &sessionMetadata) == nil {
								sessionIDs = append(sessionIDs, sessionMetadata.SessionID)
								// Use first session for Agent, SessionID, CreatedAt, IsTask, ToolUseID
								if i == 0 {
									info.Agent = sessionMetadata.Agent
									info.SessionID = sessionMetadata.SessionID
									info.CreatedAt = sessionMetadata.CreatedAt
									info.IsTask = sessionMetadata.IsTask
									info.ToolUseID = sessionMetadata.ToolUseID
								}
							}
						}
					}
				}
			}
			info.SessionIDs = sessionIDs

			return info, nil
		}
	}

	// Fall back to parsing as CheckpointInfo (old format or direct info)
	var metadata CheckpointInfo
	if err := json.Unmarshal([]byte(content), &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &metadata, nil
}

// GetMetadataBranchTree returns the tree object for the entire/checkpoints/v1 branch.
func GetMetadataBranchTree(repo *git.Repository) (*object.Tree, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch reference: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get metadata branch tree: %w", err)
	}
	return tree, nil
}

// ExtractFirstPrompt extracts and truncates the first meaningful prompt from prompt content.
// Prompts are separated by "\n\n---\n\n". Skips empty prompts and separator-only content.
// Returns empty string if no valid prompt is found.
func ExtractFirstPrompt(content string) string {
	if content == "" {
		return ""
	}

	// Prompts are separated by "\n\n---\n\n"
	// Find the first non-empty prompt
	prompts := strings.Split(content, "\n\n---\n\n")
	var firstPrompt string
	for _, p := range prompts {
		cleaned := strings.TrimSpace(p)
		// Skip empty prompts or prompts that are just dashes/separators
		if cleaned == "" || isOnlySeparators(cleaned) {
			continue
		}
		firstPrompt = cleaned
		break
	}

	if firstPrompt == "" {
		return ""
	}

	return TruncateDescription(firstPrompt, MaxDescriptionLength)
}

// ReadSessionPromptFromTree reads the first meaningful prompt from a checkpoint's prompt.txt file in a git tree.
// Returns an empty string if the prompt cannot be read.
func ReadSessionPromptFromTree(tree *object.Tree, checkpointPath string) string {
	promptPath := checkpointPath + "/" + paths.PromptFileName
	file, err := tree.File(promptPath)
	if err != nil {
		return ""
	}

	content, err := file.Contents()
	if err != nil {
		return ""
	}

	return ExtractFirstPrompt(content)
}

// isOnlySeparators checks if a string contains only dashes, spaces, and newlines.
func isOnlySeparators(s string) bool {
	for _, r := range s {
		if r != '-' && r != ' ' && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

// ReadAllSessionPromptsFromTree reads the first prompt for all sessions in a multi-session checkpoint.
// Returns a slice of prompts parallel to sessionIDs (oldest to newest).
// For single-session checkpoints, returns a slice with just the root prompt.
func ReadAllSessionPromptsFromTree(tree *object.Tree, checkpointPath string, sessionCount int, sessionIDs []string) []string {
	if sessionCount <= 1 || len(sessionIDs) <= 1 {
		// Single session - just return the root prompt
		prompt := ReadSessionPromptFromTree(tree, checkpointPath)
		if prompt != "" {
			return []string{prompt}
		}
		return nil
	}

	// Multi-session: read prompts from archived folders (0/, 1/, etc.) and root
	prompts := make([]string, len(sessionIDs))

	// Read archived session prompts (folders 0, 1, ... N-2)
	for i := range sessionCount - 1 {
		archivedPath := fmt.Sprintf("%s/%d", checkpointPath, i)
		prompts[i] = ReadSessionPromptFromTree(tree, archivedPath)
	}

	// Read the most recent session prompt (at root level)
	prompts[len(prompts)-1] = ReadSessionPromptFromTree(tree, checkpointPath)

	return prompts
}

// ReadSessionPromptFromShadow reads the first prompt for a session from the shadow branch.
// Returns an empty string if the prompt cannot be read.
func ReadSessionPromptFromShadow(repo *git.Repository, baseCommit, worktreeID, sessionID string) string {
	// Get shadow branch for this base commit using worktree-specific naming
	shadowBranchName := checkpoint.ShadowBranchNameForCommit(baseCommit, worktreeID)
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true)
	if err != nil {
		return ""
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return ""
	}

	tree, err := commit.Tree()
	if err != nil {
		return ""
	}

	// Build the path to prompt.txt: .entire/metadata/<session-id>/prompt.txt
	checkpointPath := paths.EntireMetadataDir + "/" + sessionID
	return ReadSessionPromptFromTree(tree, checkpointPath)
}

// GetRemoteMetadataBranchTree returns the tree object for origin/entire/checkpoints/v1.
func GetRemoteMetadataBranchTree(repo *git.Repository) (*object.Tree, error) {
	refName := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata branch reference: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata branch commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get remote metadata branch tree: %w", err)
	}
	return tree, nil
}

// OpenRepository opens the git repository with linked worktree support enabled.
// It uses git.PlainOpenWithOptions with EnableDotGitCommonDir set to true,
// which is required for proper operation in git worktrees created via 'git worktree add'.
//
// Without EnableDotGitCommonDir, go-git operations in worktrees can silently fail:
// - Commits appear to succeed but are not persisted
// - Refs are written to incorrect locations
// - The worktree's HEAD/index don't get updated properly
//
// This happens because worktrees use .git as a file (pointing to the main repo)
// rather than a directory, and go-git needs to route paths correctly between
// shared (.git/) and per-worktree (.git/worktrees/<name>/) locations.
//
// The function first uses 'git rev-parse --show-toplevel' to find the repository
// root, which works correctly even when called from a subdirectory within the repo.
func OpenRepository() (*git.Repository, error) {
	// First, find the repository root using git rev-parse --show-toplevel
	// This works correctly from any subdirectory within the repository
	repoRoot, err := GetWorktreePath()
	if err != nil {
		// Fallback to current directory if git command fails
		// (e.g., if git is not installed or we're not in a repo)
		repoRoot = "."
	}

	repo, err := git.PlainOpenWithOptions(repoRoot, &git.PlainOpenOptions{
		EnableDotGitCommonDir: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}
	return repo, nil
}

// IsInsideWorktree returns true if the current directory is inside a git worktree
// (as opposed to the main repository). Worktrees have .git as a file pointing
// to the main repo, while the main repo has .git as a directory.
// This function works correctly from any subdirectory within the repository.
func IsInsideWorktree() bool {
	// First find the repository root
	repoRoot, err := GetWorktreePath()
	if err != nil {
		return false
	}

	gitPath := filepath.Join(repoRoot, gitDir)
	gitInfo, err := os.Stat(gitPath)
	if err != nil {
		return false
	}
	return !gitInfo.IsDir()
}

// GetMainRepoRoot returns the root directory of the main repository.
// In the main repo, this is the worktree path (repo root).
// In a worktree, this parses the .git file to find the main repo.
// This function works correctly from any subdirectory within the repository.
//
// Per gitrepository-layout(5), a worktree's .git file is a "gitfile" containing
// "gitdir: <path>" pointing to $GIT_DIR/worktrees/<id> in the main repository.
// See: https://git-scm.com/docs/gitrepository-layout
func GetMainRepoRoot() (string, error) {
	// First find the worktree/repo root
	repoRoot, err := GetWorktreePath()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree path: %w", err)
	}

	if !IsInsideWorktree() {
		return repoRoot, nil
	}

	// Worktree .git file contains: "gitdir: /path/to/main/.git/worktrees/<id>"
	gitFilePath := filepath.Join(repoRoot, gitDir)
	content, err := os.ReadFile(gitFilePath) //nolint:gosec // G304: gitFilePath is constructed from repo root, not user input
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}

	gitdir := strings.TrimSpace(string(content))
	gitdir = strings.TrimPrefix(gitdir, "gitdir: ")

	// Extract main repo root: everything before "/.git/"
	idx := strings.LastIndex(gitdir, "/.git/")
	if idx < 0 {
		return "", fmt.Errorf("unexpected gitdir format: %s", gitdir)
	}
	return gitdir[:idx], nil
}

// GetGitCommonDir returns the path to the shared git directory.
// In a regular checkout, this is .git/
// In a worktree, this is the main repo's .git/ (not .git/worktrees/<name>/)
// Uses git rev-parse --git-common-dir for reliable handling of worktrees.
func GetGitCommonDir() (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	cmd.Dir = "."
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}

	commonDir := strings.TrimSpace(string(output))

	// git rev-parse --git-common-dir returns relative paths from the working directory,
	// so we need to make it absolute if it isn't already
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(".", commonDir)
	}

	return filepath.Clean(commonDir), nil
}

// GetWorktreePath returns the absolute path to the current worktree root.
// This is the working directory path, not the git directory.
func GetWorktreePath() (string, error) {
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree path: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// EnsureEntireGitignore ensures all required entries are in .entire/.gitignore
// Works correctly from any subdirectory within the repository.
func EnsureEntireGitignore() error {
	// Get absolute path for the gitignore file
	gitignoreAbs, err := paths.AbsPath(entireGitignore)
	if err != nil {
		gitignoreAbs = entireGitignore // Fallback to relative
	}

	// Read existing content
	var content string
	if data, err := os.ReadFile(gitignoreAbs); err == nil { //nolint:gosec // path is from AbsPath or constant
		content = string(data)
	}

	// All entries that should be in .entire/.gitignore
	requiredEntries := []string{
		"tmp/",
		"settings.local.json",
		"metadata/",
		"logs/",
	}

	// Track what needs to be added
	var toAdd []string
	for _, entry := range requiredEntries {
		if !strings.Contains(content, entry) {
			toAdd = append(toAdd, entry)
		}
	}

	// Nothing to add
	if len(toAdd) == 0 {
		return nil
	}

	// Ensure .entire directory exists
	if err := os.MkdirAll(filepath.Dir(gitignoreAbs), 0o750); err != nil {
		return fmt.Errorf("failed to create .entire directory: %w", err)
	}

	// Append missing entries to gitignore
	var sb strings.Builder
	for _, entry := range toAdd {
		sb.WriteString(entry + "\n")
	}
	content += sb.String()

	if err := os.WriteFile(gitignoreAbs, []byte(content), 0o644); err != nil { //nolint:gosec // path is from AbsPath or constant
		return fmt.Errorf("failed to write gitignore: %w", err)
	}
	return nil
}

// checkCanRewind checks if working directory is clean enough for rewind.
// Returns (canRewind, reason, error). Shared by shadow and linear-shadow strategies.
func checkCanRewind() (bool, string, error) {
	repo, err := OpenRepository()
	if err != nil {
		return false, "", fmt.Errorf("failed to open git repository: %w", err)
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return false, "", fmt.Errorf("failed to get worktree: %w", err)
	}

	status, err := worktree.Status()
	if err != nil {
		return false, "", fmt.Errorf("failed to get status: %w", err)
	}

	if status.IsClean() {
		return true, "", nil
	}

	var modified, added, deleted []string
	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Skip untracked files
		if st.Worktree == git.Untracked {
			continue
		}

		switch {
		case st.Staging == git.Added || st.Worktree == git.Added:
			added = append(added, file)
		case st.Staging == git.Deleted || st.Worktree == git.Deleted:
			deleted = append(deleted, file)
		case st.Staging == git.Modified || st.Worktree == git.Modified:
			modified = append(modified, file)
		}
	}

	if len(modified) == 0 && len(added) == 0 && len(deleted) == 0 {
		return true, "", nil
	}

	var msg strings.Builder
	msg.WriteString("You have uncommitted changes:\n")
	for _, f := range modified {
		msg.WriteString(fmt.Sprintf("  modified:   %s\n", f))
	}
	for _, f := range added {
		msg.WriteString(fmt.Sprintf("  added:      %s\n", f))
	}
	for _, f := range deleted {
		msg.WriteString(fmt.Sprintf("  deleted:    %s\n", f))
	}
	msg.WriteString("\nPlease commit or stash your changes before rewinding.")

	return false, msg.String(), nil
}

// checkCanRewindWithWarning checks working directory and returns a warning with diff stats.
// Unlike checkCanRewind, this always returns canRewind=true but includes a warning message
// with +/- line stats for uncommitted changes. Used by manual-commit strategy.
func checkCanRewindWithWarning() (bool, string, error) {
	repo, err := OpenRepository()
	if err != nil {
		// Can't open repo - still allow rewind but without stats
		return true, "", nil //nolint:nilerr // Rewind allowed even if repo can't be opened
	}

	worktree, err := repo.Worktree()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if worktree can't be accessed
	}

	status, err := worktree.Status()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if status can't be retrieved
	}

	if status.IsClean() {
		return true, "", nil
	}

	// Get HEAD commit tree for comparison - if we can't get it, just return without stats
	head, err := repo.Head()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even without HEAD (e.g., empty repo)
	}

	headCommit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if commit lookup fails
	}

	headTree, err := headCommit.Tree()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if tree lookup fails
	}

	type fileChange struct {
		status   string // "modified", "added", "deleted"
		added    int
		removed  int
		filename string
	}

	var changes []fileChange
	// Use repo root, not cwd - git status returns paths relative to repo root
	repoRoot, err := GetWorktreePath()
	if err != nil {
		return true, "", nil //nolint:nilerr // Rewind allowed even if repo root lookup fails
	}

	for file, st := range status {
		// Skip .entire directory
		if paths.IsInfrastructurePath(file) {
			continue
		}

		// Skip untracked files
		if st.Worktree == git.Untracked {
			continue
		}

		var change fileChange
		change.filename = file

		switch {
		case st.Staging == git.Added || st.Worktree == git.Added:
			change.status = "added"
			// New file - count all lines as added
			absPath := filepath.Join(repoRoot, file)
			if content, err := os.ReadFile(absPath); err == nil { //nolint:gosec // absPath is repo root + relative path from git status
				change.added = countLines(content)
			}
		case st.Staging == git.Deleted || st.Worktree == git.Deleted:
			change.status = "deleted"
			// Deleted file - count lines from HEAD as removed
			if entry, err := headTree.File(file); err == nil {
				if content, err := entry.Contents(); err == nil {
					change.removed = countLines([]byte(content))
				}
			}
		case st.Staging == git.Modified || st.Worktree == git.Modified:
			change.status = "modified"
			// Modified file - compute diff stats
			var headContent, workContent []byte
			if entry, err := headTree.File(file); err == nil {
				if content, err := entry.Contents(); err == nil {
					headContent = []byte(content)
				}
			}
			absPath := filepath.Join(repoRoot, file)
			if content, err := os.ReadFile(absPath); err == nil { //nolint:gosec // absPath is repo root + relative path from git status
				workContent = content
			}
			if headContent != nil && workContent != nil {
				change.added, change.removed = computeDiffStats(headContent, workContent)
			}
		default:
			continue
		}

		changes = append(changes, change)
	}

	if len(changes) == 0 {
		return true, "", nil
	}

	// Sort changes by filename for consistent output
	sort.Slice(changes, func(i, j int) bool {
		return changes[i].filename < changes[j].filename
	})

	var msg strings.Builder
	msg.WriteString("The following uncommitted changes will be reverted:\n")

	totalAdded, totalRemoved := 0, 0
	for _, c := range changes {
		totalAdded += c.added
		totalRemoved += c.removed

		var stats string
		switch {
		case c.added > 0 && c.removed > 0:
			stats = fmt.Sprintf("+%d/-%d", c.added, c.removed)
		case c.added > 0:
			stats = fmt.Sprintf("+%d", c.added)
		case c.removed > 0:
			stats = fmt.Sprintf("-%d", c.removed)
		}

		msg.WriteString(fmt.Sprintf("  %-10s %s", c.status+":", c.filename))
		if stats != "" {
			msg.WriteString(fmt.Sprintf(" (%s)", stats))
		}
		msg.WriteString("\n")
	}

	if totalAdded > 0 || totalRemoved > 0 {
		msg.WriteString(fmt.Sprintf("\nTotal: +%d/-%d lines\n", totalAdded, totalRemoved))
	}

	return true, msg.String(), nil
}

// countLines counts the number of lines in content.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	count := 1
	for _, b := range content {
		if b == '\n' {
			count++
		}
	}
	// Don't count trailing newline as extra line
	if len(content) > 0 && content[len(content)-1] == '\n' {
		count--
	}
	return count
}

// computeDiffStats computes added and removed line counts between old and new content.
// Uses a simple line-based diff algorithm.
func computeDiffStats(oldContent, newContent []byte) (added, removed int) {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	// Build a set of old lines with counts
	oldSet := make(map[string]int)
	for _, line := range oldLines {
		oldSet[line]++
	}

	// Check which new lines are truly new
	for _, line := range newLines {
		if oldSet[line] > 0 {
			oldSet[line]--
		} else {
			added++
		}
	}

	// Remaining old lines are removed
	for _, count := range oldSet {
		removed += count
	}

	return added, removed
}

// splitLines splits content into lines, preserving empty lines.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := string(content)
	// Remove trailing newline to avoid empty last element
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// fileExists checks if a file exists at the given path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// StageFilesContext describes what type of staging operation this is (for messages).
type StageFilesContext string

const (
	// StageForSession is used when staging files for a session checkpoint.
	StageForSession StageFilesContext = "session"
	// StageForTask is used when staging files for a task checkpoint.
	StageForTask StageFilesContext = "task"
)

// StageFiles stages modified, new, and deleted files to the git worktree.
//
// This function handles three categories of file changes:
//  1. Modified files: existing files that have been changed
//  2. New files: files that were created during the session
//  3. Deleted files: files that were removed during the session
//
// Error Handling Strategy:
//   - Individual file staging errors are logged to stderr but don't fail the operation
//   - This ensures that partial staging succeeds even if some files have issues
//   - If a modified file no longer exists, it's treated as a deletion
//
// The stageCtx parameter is used for user-facing messages to indicate whether
// this is staging for a session checkpoint or a task checkpoint.
func StageFiles(worktree *git.Worktree, modified, newFiles, deleted []string, stageCtx StageFilesContext) {
	// Get repo root for resolving file paths
	// This is critical because fileExists() uses os.Stat() which resolves relative to CWD,
	// but worktree.Add/Remove resolve relative to repo root. If CWD != repo root,
	// fileExists() could return false for existing files, causing worktree.Remove()
	// to incorrectly delete them.
	repoRoot := worktree.Filesystem.Root()

	// Stage modified files
	for _, file := range modified {
		// Resolve path relative to repo root for existence check
		absPath := filepath.Join(repoRoot, file)
		if fileExists(absPath) {
			if _, err := worktree.Add(file); err != nil {
				fmt.Fprintf(os.Stderr, "  Failed to stage %s: %v\n", file, err)
			} else {
				fmt.Fprintf(os.Stderr, "  Staged: %s\n", file)
			}
		} else {
			// File was deleted - stage the deletion
			if _, err := worktree.Remove(file); err != nil {
				fmt.Fprintf(os.Stderr, "  File not found or deleted: %s\n", file)
			}
		}
	}

	// Stage new files
	if len(newFiles) > 0 {
		fmt.Fprintf(os.Stderr, "Staging %d new files created during %s:\n", len(newFiles), stageCtx)
		for _, file := range newFiles {
			if _, err := worktree.Add(file); err != nil {
				fmt.Fprintf(os.Stderr, "  Failed to stage %s: %v\n", file, err)
			} else {
				fmt.Fprintf(os.Stderr, "  Staged new file: %s\n", file)
			}
		}
	}

	// Stage deleted files
	for _, file := range deleted {
		if _, err := worktree.Remove(file); err != nil {
			fmt.Fprintf(os.Stderr, "  Failed to stage deleted file %s: %v\n", file, err)
		} else {
			fmt.Fprintf(os.Stderr, "  Staged deleted file: %s\n", file)
		}
	}
}

// getTaskCheckpointFromTree retrieves a task checkpoint from a commit tree.
// Shared implementation for shadow and linear-shadow strategies.
func getTaskCheckpointFromTree(point RewindPoint) (*TaskCheckpoint, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// Read checkpoint.json from the tree
	checkpointPath := point.MetadataDir + "/checkpoint.json"
	file, err := tree.File(checkpointPath)
	if err != nil {
		return nil, fmt.Errorf("failed to find checkpoint at %s: %w", checkpointPath, err)
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint: %w", err)
	}

	var checkpoint TaskCheckpoint
	if err := json.Unmarshal([]byte(content), &checkpoint); err != nil {
		return nil, fmt.Errorf("failed to parse checkpoint: %w", err)
	}

	return &checkpoint, nil
}

// getTaskTranscriptFromTree retrieves a task transcript from a commit tree.
// Shared implementation for shadow and linear-shadow strategies.
func getTaskTranscriptFromTree(point RewindPoint) ([]byte, error) {
	if !point.IsTaskCheckpoint {
		return nil, ErrNotTaskCheckpoint
	}

	repo, err := OpenRepository()
	if err != nil {
		return nil, fmt.Errorf("failed to open repository: %w", err)
	}

	commitHash := plumbing.NewHash(point.ID)
	commit, err := repo.CommitObject(commitHash)
	if err != nil {
		return nil, fmt.Errorf("failed to get commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get tree: %w", err)
	}

	// MetadataDir format: .entire/metadata/<session>/tasks/<toolUseID>
	// Session transcript is at: .entire/metadata/<session>/<TranscriptFileName>
	sessionDir := filepath.Dir(filepath.Dir(point.MetadataDir))

	// Try current format first, then legacy
	transcriptPath := sessionDir + "/" + paths.TranscriptFileName
	file, err := tree.File(transcriptPath)
	if err != nil {
		transcriptPath = sessionDir + "/" + paths.TranscriptFileNameLegacy
		file, err = tree.File(transcriptPath)
		if err != nil {
			return nil, fmt.Errorf("failed to find transcript: %w", err)
		}
	}

	content, err := file.Contents()
	if err != nil {
		return nil, fmt.Errorf("failed to read transcript: %w", err)
	}

	return []byte(content), nil
}

// HardResetWithProtection performs a git reset --hard to the specified commit.
// Uses the git CLI instead of go-git because go-git's HardReset incorrectly
// deletes untracked directories (like .entire/) even when they're in .gitignore.
// Returns the short commit ID (7 chars) on success for display purposes.
func HardResetWithProtection(commitHash plumbing.Hash) (shortID string, err error) {
	ctx := context.Background()
	hashStr := commitHash.String()
	cmd := exec.CommandContext(ctx, "git", "reset", "--hard", hashStr) //nolint:gosec // hashStr is a plumbing.Hash, not user input
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("reset failed: %s: %w", strings.TrimSpace(string(output)), err)
	}

	// Return short commit ID for display
	shortID = hashStr
	if len(shortID) > 7 {
		shortID = shortID[:7]
	}
	return shortID, nil
}

// collectUntrackedFiles collects all untracked files in the working directory.
// This is used to capture the initial state when starting a session,
// ensuring untracked files present at session start are preserved during rewind.
// Excludes .git/, .entire/, and .claude/ directories (which are system/config directories).
// Works correctly from any subdirectory within the repository.
func collectUntrackedFiles() ([]string, error) {
	// Get repository root to walk from there
	repoRoot, err := GetWorktreePath()
	if err != nil {
		repoRoot = "." // Fallback to current directory
	}

	var untrackedFiles []string
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // Skip filesystem errors during walk
		}

		// Get path relative to repo root
		relPath, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return nil //nolint:nilerr // Skip paths we can't make relative
		}

		// Skip directories
		if info.IsDir() {
			// Skip .git, .claude, and .entire directories
			if relPath == gitDir || relPath == claudeDir || relPath == entireDir ||
				strings.HasPrefix(relPath, gitDir+"/") || strings.HasPrefix(relPath, claudeDir+"/") || strings.HasPrefix(relPath, entireDir+"/") {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip files in special directories (shouldn't reach here due to SkipDir, but safety check)
		if strings.HasPrefix(relPath, gitDir+"/") || strings.HasPrefix(relPath, claudeDir+"/") || strings.HasPrefix(relPath, entireDir+"/") {
			return nil
		}

		untrackedFiles = append(untrackedFiles, relPath)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk directory: %w", err)
	}

	return untrackedFiles, nil
}

// ExtractSessionIDFromCommit extracts the session ID from a commit's trailers.
// It checks the Entire-Session trailer first, then falls back to extracting from
// the metadata directory path in the Entire-Metadata trailer.
// Returns empty string if no session ID is found.
func ExtractSessionIDFromCommit(commit *object.Commit) string {
	// Try Entire-Session trailer first
	if sessionID, found := trailers.ParseSession(commit.Message); found {
		return sessionID
	}

	// Try extracting from metadata directory (last path component)
	if metadataDir, found := trailers.ParseMetadata(commit.Message); found {
		return filepath.Base(metadataDir)
	}

	return ""
}

// NOTE: The following git tree helper functions have been moved to checkpoint/ package:
// - FlattenTree -> checkpoint.FlattenTree
// - CreateBlobFromContent -> checkpoint.CreateBlobFromContent
// - BuildTreeFromEntries -> checkpoint.BuildTreeFromEntries
// - sortTreeEntries (internal to checkpoint package)
// - treeNode, insertIntoTree, buildTreeObject (internal to checkpoint package)
//
// See push_common.go and session_test.go for usage examples.

// createCommit creates a commit object
func createCommit(repo *git.Repository, treeHash, parentHash plumbing.Hash, message, authorName, authorEmail string) (plumbing.Hash, error) {
	now := time.Now()
	sig := object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  now,
	}

	commit := &object.Commit{
		TreeHash:  treeHash,
		Author:    sig,
		Committer: sig,
		Message:   message,
	}

	// Add parent if not a new branch
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	obj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to encode commit: %w", err)
	}

	hash, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to store commit: %w", err)
	}

	return hash, nil
}

// getSessionDescriptionFromTree reads the first line of prompt.txt or context.md from a git tree.
// This is the tree-based equivalent of getSessionDescription (which reads from filesystem).
//
// If metadataDir is provided, looks for files at metadataDir/prompt.txt or metadataDir/context.md.
// If metadataDir is empty, first tries the root of the tree (for when the tree is already
// the session directory, e.g., auto-commit strategy's sharded metadata), then falls back to
// searching for .entire/metadata/*/prompt.txt or context.md (for full worktree trees).
func getSessionDescriptionFromTree(tree *object.Tree, metadataDir string) string {
	// Helper to read first line from a file in tree
	readFirstLine := func(path string) string {
		file, err := tree.File(path)
		if err != nil {
			return ""
		}
		content, err := file.Contents()
		if err != nil {
			return ""
		}
		lines := strings.SplitN(content, "\n", 2)
		if len(lines) > 0 && lines[0] != "" {
			desc := strings.TrimSpace(lines[0])
			// Remove markdown header prefix if present
			return strings.TrimPrefix(desc, "# ")
		}
		return ""
	}

	// If metadataDir is provided, look there directly
	if metadataDir != "" {
		if desc := readFirstLine(metadataDir + "/" + paths.PromptFileName); desc != "" {
			return desc
		}
		if desc := readFirstLine(metadataDir + "/" + paths.ContextFileName); desc != "" {
			return desc
		}
		return NoDescription
	}

	// No metadataDir provided - first try looking at the root of the tree
	// (used when the tree is already the session directory)
	if desc := readFirstLine(paths.PromptFileName); desc != "" {
		return desc
	}
	if desc := readFirstLine(paths.ContextFileName); desc != "" {
		return desc
	}

	// Fall back to searching for .entire/metadata/*/prompt.txt or context.md
	// (used when the tree is the full worktree)
	var desc string
	//nolint:errcheck // We ignore errors here as we're just searching for a description
	_ = tree.Files().ForEach(func(f *object.File) error {
		if desc != "" {
			return nil // Already found description
		}
		name := f.Name
		if strings.Contains(name, ".entire/metadata/") {
			if strings.HasSuffix(name, "/"+paths.PromptFileName) || strings.HasSuffix(name, "/"+paths.ContextFileName) {
				content, err := f.Contents()
				if err != nil {
					return nil //nolint:nilerr // Skip files we can't read, continue searching
				}
				lines := strings.SplitN(content, "\n", 2)
				if len(lines) > 0 && lines[0] != "" {
					desc = strings.TrimSpace(lines[0])
					desc = strings.TrimPrefix(desc, "# ")
				}
			}
		}
		return nil
	})

	if desc != "" {
		return desc
	}
	return NoDescription
}

// GetGitAuthorFromRepo retrieves the git user.name and user.email from the repository config.
// It checks local config first, then falls back to global config.
// Returns ("Unknown", "unknown@local") if no user is configured - this allows
// operations to proceed even without git user config, which is especially useful
// for internal metadata commits on branches like entire/checkpoints/v1.
func GetGitAuthorFromRepo(repo *git.Repository) (name, email string) {
	// Get repository config (includes local settings)
	cfg, err := repo.Config()
	if err == nil {
		name = cfg.User.Name
		email = cfg.User.Email
	}

	// If not found in local config, try global config
	if name == "" || email == "" {
		globalCfg, err := config.LoadConfig(config.GlobalScope)
		if err == nil {
			if name == "" {
				name = globalCfg.User.Name
			}
			if email == "" {
				email = globalCfg.User.Email
			}
		}
	}

	// Provide sensible defaults if git user is not configured
	if name == "" {
		name = "Unknown"
	}
	if email == "" {
		email = "unknown@local"
	}

	return name, email
}

// GetCurrentBranchName returns the short name of the current branch if HEAD points to a branch.
// Returns an empty string if in detached HEAD state or if there's an error reading HEAD.
// This is used to capture branch metadata for checkpoints.
func GetCurrentBranchName(repo *git.Repository) string {
	head, err := repo.Head()
	if err != nil || !head.Name().IsBranch() {
		return ""
	}
	return head.Name().Short()
}

// getMainBranchHash returns the hash of the main branch (main or master).
// Returns ZeroHash if no main branch is found.
func GetMainBranchHash(repo *git.Repository) plumbing.Hash {
	// Try common main branch names
	for _, branchName := range []string{branchMain, branchMaster} {
		// Try local branch first
		ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
		if err == nil {
			return ref.Hash()
		}
		// Try remote tracking branch
		ref, err = repo.Reference(plumbing.NewRemoteReferenceName("origin", branchName), true)
		if err == nil {
			return ref.Hash()
		}
	}
	return plumbing.ZeroHash
}

// GetDefaultBranchName returns the name of the default branch.
// First checks origin/HEAD, then falls back to checking if main/master exists.
// Returns empty string if unable to determine.
// NOTE: Duplicated from cli/git_operations.go - see ENT-129 for consolidation.
func GetDefaultBranchName(repo *git.Repository) string {
	// Try to get the symbolic reference for origin/HEAD
	// Use resolved=false to get the symbolic ref itself, then extract its target
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), false)
	if err == nil && ref != nil && ref.Type() == plumbing.SymbolicReference {
		target := ref.Target().String()
		if branchName, found := strings.CutPrefix(target, "refs/remotes/origin/"); found {
			return branchName
		}
	}

	// Fallback: check if origin/main or origin/master exists
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchMain), true); err == nil {
		return branchMain
	}
	if _, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branchMaster), true); err == nil {
		return branchMaster
	}

	// Final fallback: check local branches
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(branchMain), true); err == nil {
		return branchMain
	}
	if _, err := repo.Reference(plumbing.NewBranchReferenceName(branchMaster), true); err == nil {
		return branchMaster
	}

	return ""
}

// IsOnDefaultBranch checks if the repository HEAD is on the default branch.
// Returns (isOnDefault, currentBranchName).
// NOTE: Duplicated from cli/git_operations.go - see ENT-129 for consolidation.
func IsOnDefaultBranch(repo *git.Repository) (bool, string) {
	currentBranch := GetCurrentBranchName(repo)
	if currentBranch == "" {
		return false, ""
	}

	defaultBranch := GetDefaultBranchName(repo)
	if defaultBranch == "" {
		// Can't determine default, check common names
		if currentBranch == branchMain || currentBranch == branchMaster {
			return true, currentBranch
		}
		return false, currentBranch
	}

	return currentBranch == defaultBranch, currentBranch
}
