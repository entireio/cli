package paths

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Directory constants
const (
	EntireDir         = ".entire"
	EntireTmpDir      = ".entire/tmp"
	EntireMetadataDir = ".entire/metadata"
)

// Metadata file names
const (
	PromptFileName           = "prompt.txt"
	TranscriptFileName       = "full.jsonl"
	TranscriptFileNameLegacy = "full.log"
	MetadataFileName         = "metadata.json"
	CheckpointFileName       = "checkpoint.json"
	ContentHashFileName      = "content_hash.txt"
	SettingsFileName         = "settings.json"
)

// DefaultMetadataBranchName is the default orphan branch used by manual-commit strategy to store metadata.
const DefaultMetadataBranchName = "entire/checkpoints/v1"

// MetadataBranchName is the active metadata branch name.
// Defaults to DefaultMetadataBranchName ("entire/checkpoints/v1").
// Override via ConfigureBranchPrefix() to use a custom prefix from settings.
var MetadataBranchName = DefaultMetadataBranchName

// MetadataBranchNameFromPrefix returns the metadata branch name for a given branch prefix.
// Example: "jfrog/" → "jfrog/checkpoints/v1"
func MetadataBranchNameFromPrefix(branchPrefix string) string {
	return branchPrefix + "checkpoints/v1"
}

// ConfigureBranchPrefix sets the active metadata branch name based on the given prefix.
// Call this early during CLI startup with the branch_prefix from settings.
func ConfigureBranchPrefix(branchPrefix string) {
	MetadataBranchName = branchPrefix + "checkpoints/v1"
}

// TrailsBranchName is the orphan branch used to store trail metadata.
// Trails are branch-centric work tracking abstractions that link to checkpoints by branch name.
const TrailsBranchName = "entire/trails/v1"

// CheckpointPath returns the sharded storage path for a checkpoint ID.
// Uses first 2 characters as shard (256 buckets), remaining as folder name.
// Example: "a3b2c4d5e6f7" -> "a3/b2c4d5e6f7"
//
// Deprecated: Use checkpointID.Path() directly instead.
func CheckpointPath(checkpointID id.CheckpointID) string {
	return checkpointID.Path()
}

// worktreeRootCache caches the worktree root to avoid repeated git commands.
// The cache is keyed by the current working directory to handle directory changes.
var (
	worktreeRootMu       sync.RWMutex
	worktreeRootCache    string
	worktreeRootCacheDir string
)

// WorktreeRoot returns the git worktree root directory.
// Uses 'git rev-parse --show-toplevel' which returns the working tree toplevel.
// In a worktree this is the worktree root, not the main repository root.
// The result is cached per working directory.
// Returns an error if not inside a git repository.
func WorktreeRoot(ctx context.Context) (string, error) {
	// Get current working directory to check cache validity
	cwd, err := os.Getwd() //nolint:forbidigo // already present in codebase
	if err != nil {
		cwd = ""
	}

	// Check cache with read lock first
	worktreeRootMu.RLock()
	if worktreeRootCache != "" && worktreeRootCacheDir == cwd {
		cached := worktreeRootCache
		worktreeRootMu.RUnlock()
		return cached, nil
	}
	worktreeRootMu.RUnlock()

	// Cache miss - get worktree root and update cache with write lock
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git worktree root: %w", err)
	}

	root := strings.TrimSpace(string(output))

	worktreeRootMu.Lock()
	worktreeRootCache = root
	worktreeRootCacheDir = cwd
	worktreeRootMu.Unlock()

	return root, nil
}

// ClearWorktreeRootCache clears the cached worktree root.
// This is primarily useful for testing when changing directories.
func ClearWorktreeRootCache() {
	worktreeRootMu.Lock()
	worktreeRootCache = ""
	worktreeRootCacheDir = ""
	worktreeRootMu.Unlock()
}

// AbsPath returns the absolute path for a relative path within the repository.
// If the path is already absolute, it is returned as-is.
// Uses WorktreeRoot() to resolve paths relative to the worktree root.
func AbsPath(ctx context.Context, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return relPath, nil
	}

	root, err := WorktreeRoot(ctx)
	if err != nil {
		return "", err
	}

	return filepath.Join(root, relPath), nil
}

// IsInfrastructurePath returns true if the path is part of CLI infrastructure
// (i.e., inside the .entire directory)
func IsInfrastructurePath(path string) bool {
	return strings.HasPrefix(path, EntireDir+"/") || path == EntireDir
}

// ToRelativePath converts an absolute path to relative.
// Returns empty string if the path is outside the working directory.
func ToRelativePath(absPath, cwd string) string {
	if !filepath.IsAbs(absPath) {
		return absPath
	}
	relPath, err := filepath.Rel(cwd, absPath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		return ""
	}
	return relPath
}

// nonAlphanumericRegex matches any non-alphanumeric character
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9]`)

// SanitizePathForClaude converts a path to Claude's project directory format.
// Claude replaces any non-alphanumeric character with a dash.
func SanitizePathForClaude(path string) string {
	return nonAlphanumericRegex.ReplaceAllString(path, "-")
}

// GetClaudeProjectDir returns the directory where Claude stores session transcripts
// for the given repository path.
//
// In test environments, set ENTIRE_TEST_CLAUDE_PROJECT_DIR to override the default location.
func GetClaudeProjectDir(repoPath string) (string, error) {
	override := os.Getenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR")
	if override != "" {
		return override, nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	projectDir := SanitizePathForClaude(repoPath)
	return filepath.Join(homeDir, ".claude", "projects", projectDir), nil
}

// SessionMetadataDirFromSessionID returns the path to a session's metadata directory
// for the given Entire session ID. The sessionID must be the full, already date-prefixed
// Entire session identifier as stored on disk, not an agent-specific or raw Claude ID.
func SessionMetadataDirFromSessionID(sessionID string) string {
	return EntireMetadataDir + "/" + sessionID
}

// ExtractSessionIDFromTranscriptPath attempts to extract a session ID from a transcript path.
// Claude transcripts are stored at ~/.claude/projects/<project>/sessions/<id>.jsonl
// If the path doesn't match expected format, returns empty string.
func ExtractSessionIDFromTranscriptPath(transcriptPath string) string {
	// Try to extract from typical path: ~/.claude/projects/<project>/sessions/<id>.jsonl
	parts := strings.Split(filepath.ToSlash(transcriptPath), "/")
	for i, part := range parts {
		if part == "sessions" && i+1 < len(parts) {
			// Return filename without extension
			filename := parts[i+1]
			if strings.HasSuffix(filename, ".jsonl") {
				return strings.TrimSuffix(filename, ".jsonl")
			}
			return filename
		}
	}
	return ""
}
