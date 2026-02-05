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

	"entire.io/cli/cmd/entire/cli/checkpoint/id"
)

// Directory constants
const (
	EntireDir          = ".entire"
	EntireTmpDir       = ".entire/tmp"
	EntireMetadataDir  = ".entire/metadata"
	CurrentSessionFile = ".entire/current_session"
)

// Metadata file names
const (
	ContextFileName          = "context.md"
	PromptFileName           = "prompt.txt"
	SummaryFileName          = "summary.txt"
	TranscriptFileName       = "full.jsonl"
	TranscriptFileNameLegacy = "full.log"
	MetadataFileName         = "metadata.json"
	CheckpointFileName       = "checkpoint.json"
	ContentHashFileName      = "content_hash.txt"
	SettingsFileName         = "settings.json"
)

// MetadataBranchName is the orphan branch used by auto-commit and manual-commit strategies to store metadata
const MetadataBranchName = "entire/sessions"

// CheckpointPath returns the sharded storage path for a checkpoint ID.
// Uses first 2 characters as shard (256 buckets), remaining as folder name.
// Example: "a3b2c4d5e6f7" -> "a3/b2c4d5e6f7"
//
// Deprecated: Use checkpointID.Path() directly instead.
func CheckpointPath(checkpointID id.CheckpointID) string {
	return checkpointID.Path()
}

// repoRootCache caches the repository root to avoid repeated git commands.
// The cache is keyed by the current working directory to handle directory changes.
var (
	repoRootMu       sync.RWMutex
	repoRootCache    string
	repoRootCacheDir string
)

// RepoRoot returns the git repository root directory.
// Uses 'git rev-parse --show-toplevel' which works from any subdirectory.
// The result is cached per working directory.
// Returns an error if not inside a git repository.
//
// Note: This duplicates gitutil.GetWorktreePath() to keep paths package
// independent of git-specific packages.
func RepoRoot() (string, error) {
	// Get current working directory to check cache validity
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}

	// Check cache with read lock first
	repoRootMu.RLock()
	if repoRootCache != "" && repoRootCacheDir == cwd {
		cached := repoRootCache
		repoRootMu.RUnlock()
		return cached, nil
	}
	repoRootMu.RUnlock()

	// Cache miss - get repo root and update cache with write lock
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git repository root: %w", err)
	}

	root := strings.TrimSpace(string(output))

	repoRootMu.Lock()
	repoRootCache = root
	repoRootCacheDir = cwd
	repoRootMu.Unlock()

	return root, nil
}

// ClearRepoRootCache clears the cached repository root.
// This is primarily useful for testing when changing directories.
func ClearRepoRootCache() {
	repoRootMu.Lock()
	repoRootCache = ""
	repoRootCacheDir = ""
	repoRootMu.Unlock()
}

// RepoRootOr returns the git repository root directory, or the current directory
// if not inside a git repository. This is useful for functions that need a fallback.
func RepoRootOr(fallback string) string {
	root, err := RepoRoot()
	if err != nil {
		return fallback
	}
	return root
}

// AbsPath returns the absolute path for a relative path within the repository.
// If the path is already absolute, it is returned as-is.
// Uses RepoRoot() to resolve paths relative to the repository root.
func AbsPath(relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return relPath, nil
	}

	root, err := RepoRoot()
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

// ReadCurrentSession reads the current session ID from .entire/current_session.
// Returns an empty string (not error) if the file doesn't exist.
// Works correctly from any subdirectory within the repository.
func ReadCurrentSession() (string, error) {
	sessionFile, err := AbsPath(CurrentSessionFile)
	if err != nil {
		// Fallback to relative path if not in a git repo
		sessionFile = CurrentSessionFile
	}
	data, err := os.ReadFile(sessionFile) //nolint:gosec // path is from AbsPath or constant
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to read current session file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// WriteCurrentSession writes the session ID to .entire/current_session.
// Creates the .entire directory if it doesn't exist.
// Works correctly from any subdirectory within the repository.
func WriteCurrentSession(sessionID string) error {
	// Get absolute paths for the directory and file
	entireDirAbs, err := AbsPath(EntireDir)
	if err != nil {
		// Fallback to relative path if not in a git repo
		entireDirAbs = EntireDir
	}
	sessionFileAbs, err := AbsPath(CurrentSessionFile)
	if err != nil {
		sessionFileAbs = CurrentSessionFile
	}

	// Ensure .entire directory exists
	if err := os.MkdirAll(entireDirAbs, 0o750); err != nil {
		return fmt.Errorf("failed to create .entire directory: %w", err)
	}

	// Write session ID to file (no newline, just the ID)
	if err := os.WriteFile(sessionFileAbs, []byte(sessionID), 0o600); err != nil {
		return fmt.Errorf("failed to write current session file: %w", err)
	}

	return nil
}
