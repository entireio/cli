package paths

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
	ReadmeFileName           = "README.md"
)

// README content for directories and branches created by Entire.
// These help developers understand what these locations are for.
const (
	// EntireDirReadme is placed in .entire/ to explain its purpose.
	EntireDirReadme = `# .entire

This directory is created by [Entire](https://entire.io) to store local session state.

- Added to ` + "`.gitignore`" + ` so it is not committed to the repository
- Safe to delete - will be recreated as needed
`

	// SessionStateDirReadme is placed in .git/entire-sessions/ to explain its purpose.
	SessionStateDirReadme = `# entire-sessions

This directory is created by [Entire](https://entire.io) to track active session state.

- Located in ` + "`.git/`" + ` so it's shared across worktrees
- Safe to delete, but you'll lose ability to rewind uncommitted checkpoints
`

	// SessionsBranchReadme is placed at the root of the entire/sessions branch.
	SessionsBranchReadme = `# entire/sessions

This is an orphan branch created by [Entire](https://entire.io) to store checkpoint metadata.

- Not meant to be checked out for normal work
- Keeps session history separate from your main branch
- Used by ` + "`entire rewind`" + ` to restore previous states
- **Do not delete** - you will lose all session history and rewind capability
`
)

// Metadata trailer key used in commit messages
const MetadataTrailerKey = "Entire-Metadata"

// MetadataTaskTrailerKey is the trailer key for task checkpoint commits
const MetadataTaskTrailerKey = "Entire-Metadata-Task"

// StrategyTrailerKey is the trailer key indicating which strategy created the commit
const StrategyTrailerKey = "Entire-Strategy"

// MetadataBranchName is the orphan branch used by auto-commit and manual-commit strategies to store metadata
const MetadataBranchName = "entire/sessions"

// BaseCommitTrailerKey links shadow commits to their base code commit
const BaseCommitTrailerKey = "Base-Commit"

// SessionTrailerKey identifies which session created a commit
const SessionTrailerKey = "Entire-Session"

// CondensationTrailerKey identifies the condensation ID for a commit
const CondensationTrailerKey = "Entire-Condensation"

// SourceRefTrailerKey links code commits to their metadata on a shadow/metadata branch
// Format: "<branch>@<commit-hash>" e.g. "entire/metadata@abc123def456"
const SourceRefTrailerKey = "Entire-Source-Ref"

// CheckpointTrailerKey links commits to their checkpoint metadata on entire/sessions.
// Format: 12 hex characters e.g. "a3b2c4d5e6f7"
// This trailer survives git amend and rebase operations.
const CheckpointTrailerKey = "Entire-Checkpoint"

// EphemeralBranchTrailerKey identifies the shadow branch that a checkpoint originated from.
// Used in manual-commit strategy checkpoint commits on entire/sessions branch.
// Format: full branch name e.g. "entire/2b4c177"
const EphemeralBranchTrailerKey = "Ephemeral-branch"

// AgentTrailerKey identifies the agent that created a checkpoint.
// Format: human-readable agent name e.g. "Claude Code", "Cursor"
const AgentTrailerKey = "Entire-Agent"

// repoRootCache caches the repository root to avoid repeated git commands.
// The cache is keyed by the current working directory to handle directory changes.
var (
	repoRootCache    string
	repoRootCacheDir string
)

// RepoRoot returns the git repository root directory.
// Uses 'git rev-parse --show-toplevel' which works from any subdirectory.
// The result is cached per working directory.
// Returns an error if not inside a git repository.
func RepoRoot() (string, error) {
	// Get current working directory to check cache validity
	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}

	// Return cached value if we're in the same directory
	if repoRootCache != "" && repoRootCacheDir == cwd {
		return repoRootCache, nil
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git repository root: %w", err)
	}

	repoRootCache = strings.TrimSpace(string(output))
	repoRootCacheDir = cwd
	return repoRootCache, nil
}

// ClearRepoRootCache clears the cached repository root.
// This is primarily useful for testing when changing directories.
func ClearRepoRootCache() {
	repoRootCache = ""
	repoRootCacheDir = ""
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

// FormatSourceRefTrailer creates a trailer value linking to a metadata commit.
// Format: "<branch>@<commit-hash-prefix>"
func FormatSourceRefTrailer(branch, commitHash string) string {
	shortHash := commitHash
	if len(shortHash) > 12 {
		shortHash = shortHash[:12]
	}
	return fmt.Sprintf("%s@%s", branch, shortHash)
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

// FormatMetadataTrailer creates a commit message with metadata trailer
func FormatMetadataTrailer(message, metadataDir string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, MetadataTrailerKey, metadataDir)
}

// FormatMetadataTrailerWithStrategy creates a commit message with metadata and strategy trailers
func FormatMetadataTrailerWithStrategy(message, metadataDir, strategy string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n%s: %s\n", message, MetadataTrailerKey, metadataDir, StrategyTrailerKey, strategy)
}

// FormatShadowCommitMessage creates a commit message for manual-commit strategy checkpoints.
// Includes Entire-Metadata, Entire-Session, and Entire-Strategy trailers.
func FormatShadowCommitMessage(message, metadataDir, sessionID string) string {
	var sb strings.Builder
	sb.WriteString(message)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("%s: %s\n", MetadataTrailerKey, metadataDir))
	sb.WriteString(fmt.Sprintf("%s: %s\n", SessionTrailerKey, sessionID))
	sb.WriteString(fmt.Sprintf("%s: %s\n", StrategyTrailerKey, "manual-commit"))
	return sb.String()
}

// FormatShadowTaskCommitMessage creates a commit message for manual-commit task checkpoints.
// Includes Entire-Metadata-Task, Entire-Session, and Entire-Strategy trailers.
func FormatShadowTaskCommitMessage(message, taskMetadataDir, sessionID string) string {
	var sb strings.Builder
	sb.WriteString(message)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("%s: %s\n", MetadataTaskTrailerKey, taskMetadataDir))
	sb.WriteString(fmt.Sprintf("%s: %s\n", SessionTrailerKey, sessionID))
	sb.WriteString(fmt.Sprintf("%s: %s\n", StrategyTrailerKey, "manual-commit"))
	return sb.String()
}

// FormatStrategyTrailer creates a commit message with just the strategy trailer (no metadata dir)
func FormatStrategyTrailer(message, strategy string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, StrategyTrailerKey, strategy)
}

// ParseStrategyTrailer extracts strategy from commit message.
// Returns the strategy name and true if found, empty string and false otherwise.
func ParseStrategyTrailer(commitMessage string) (string, bool) {
	matches := strategyTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return NormalizeStrategyName(strings.TrimSpace(matches[1])), true
	}
	return "", false
}

// NormalizeStrategyName maps legacy strategy names to current names.
// These are the strategy names used during initial development and are kept for backwards
// compatibility
func NormalizeStrategyName(name string) string {
	switch name {
	case "dual":
		return "auto-commit"
	case "shadow":
		return "manual-commit"
	default:
		return name
	}
}

// ParseMetadataTrailer extracts metadata dir from commit message.
// Returns the metadata directory and true if found, empty string and false otherwise.
func ParseMetadataTrailer(commitMessage string) (string, bool) {
	matches := metadataTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// FormatTaskMetadataTrailer creates a commit message with task metadata trailer
func FormatTaskMetadataTrailer(message, taskMetadataDir string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, MetadataTaskTrailerKey, taskMetadataDir)
}

// FormatTaskMetadataTrailerWithStrategy creates a commit message with task metadata and strategy trailers
func FormatTaskMetadataTrailerWithStrategy(message, taskMetadataDir, strategy string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n%s: %s\n", message, MetadataTaskTrailerKey, taskMetadataDir, StrategyTrailerKey, strategy)
}

// ParseTaskMetadataTrailer extracts task metadata dir from commit message.
// Returns the task metadata directory and true if found, empty string and false otherwise.
func ParseTaskMetadataTrailer(commitMessage string) (string, bool) {
	matches := taskMetadataTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseBaseCommitTrailer extracts the base commit SHA from a commit message.
// Returns the full SHA and true if found, empty string and false otherwise.
func ParseBaseCommitTrailer(commitMessage string) (string, bool) {
	matches := baseCommitTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return matches[1], true
	}
	return "", false
}

// ParseCondensationTrailer extracts the condensation ID from a commit message.
// Returns the condensation ID and true if found, empty string and false otherwise.
func ParseCondensationTrailer(commitMessage string) (string, bool) {
	matches := condensationTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseSessionTrailer extracts the session ID from a commit message.
// Returns the session ID and true if found, empty string and false otherwise.
// Note: If multiple Entire-Session trailers exist, this returns only the first one.
// Use ParseAllSessionTrailers to get all session IDs.
func ParseSessionTrailer(commitMessage string) (string, bool) {
	matches := sessionTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// GenerateCheckpointID generates a unique 12-character hex checkpoint ID.
// Uses crypto/rand for secure random generation.
// Returns 12 hex characters (6 bytes = ~281 trillion unique values).
func GenerateCheckpointID() string {
	b := make([]byte, 6)
	//nolint:errcheck,gosec // crypto/rand.Read is documented to always succeed on supported platforms
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ParseCheckpointTrailer extracts the checkpoint ID from a commit message.
// Returns the checkpoint ID and true if found, empty string and false otherwise.
func ParseCheckpointTrailer(commitMessage string) (string, bool) {
	matches := checkpointTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// CheckpointPath returns the sharded path for a checkpoint ID on entire/sessions.
// Uses first 2 characters as shard (256 buckets), remaining as folder name.
// Example: "a3b2c4d5e6f7" -> "a3/b2c4d5e6f7"
func CheckpointPath(checkpointID string) string {
	if len(checkpointID) < 3 {
		return checkpointID
	}
	return checkpointID[:2] + "/" + checkpointID[2:]
}

// Pre-compiled regexes for trailer parsing (compiled at package init for performance)
var (
	// checkpointIDRegex matches valid checkpoint IDs (12 lowercase hex characters)
	checkpointIDRegex = regexp.MustCompile(`^[a-f0-9]{12}$`)

	// pathSafeRegex matches strings safe for use in file paths (no path separators or traversal)
	pathSafeRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

	// Trailer parsing regexes
	strategyTrailerRegex     = regexp.MustCompile(StrategyTrailerKey + `:\s*(.+)`)
	metadataTrailerRegex     = regexp.MustCompile(MetadataTrailerKey + `:\s*(.+)`)
	taskMetadataTrailerRegex = regexp.MustCompile(MetadataTaskTrailerKey + `:\s*(.+)`)
	baseCommitTrailerRegex   = regexp.MustCompile(BaseCommitTrailerKey + `:\s*([a-f0-9]{40})`)
	condensationTrailerRegex = regexp.MustCompile(CondensationTrailerKey + `:\s*(.+)`)
	sessionTrailerRegex      = regexp.MustCompile(SessionTrailerKey + `:\s*(.+)`)
	checkpointTrailerRegex   = regexp.MustCompile(CheckpointTrailerKey + `:\s*([a-f0-9]+)`)
)

// ValidateCheckpointID validates that a checkpoint ID is exactly 12 lowercase hex characters.
func ValidateCheckpointID(id string) error {
	if !checkpointIDRegex.MatchString(id) {
		return fmt.Errorf("invalid checkpoint ID %q: must be 12 lowercase hex characters", id)
	}
	return nil
}

// ValidateSessionID validates that a session ID is non-empty and doesn't contain path separators.
func ValidateSessionID(id string) error {
	if id == "" {
		return errors.New("session ID cannot be empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return fmt.Errorf("invalid session ID %q: contains path separators", id)
	}
	return nil
}

// ValidateToolUseID validates that a tool use ID contains only safe characters for paths.
// Tool use IDs can be UUIDs or prefixed identifiers like "toolu_xxx".
func ValidateToolUseID(id string) error {
	if id == "" {
		return nil // Empty is allowed (optional field)
	}
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid tool use ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	return nil
}

// ValidateAgentID validates that an agent ID contains only safe characters for paths.
func ValidateAgentID(id string) error {
	if id == "" {
		return nil // Empty is allowed (optional field)
	}
	if !pathSafeRegex.MatchString(id) {
		return fmt.Errorf("invalid agent ID %q: must be alphanumeric with underscores/hyphens only", id)
	}
	return nil
}

// TODO: THIS SHOULD BE REPLACED WITH ParseCheckpointTrailer ?!
// ParseAllSessionTrailers extracts all session IDs from a commit message.
// A commit can have multiple Entire-Session trailers if multiple sessions
// contributed to it. Returns a slice of session IDs (may be empty).
func ParseAllSessionTrailers(commitMessage string) []string {
	matches := sessionTrailerRegex.FindAllStringSubmatch(commitMessage, -1)

	var sessionIDs []string
	seen := make(map[string]bool)

	for _, match := range matches {
		if len(match) > 1 {
			sessionID := strings.TrimSpace(match[1])
			// Deduplicate
			if !seen[sessionID] {
				seen[sessionID] = true
				sessionIDs = append(sessionIDs, sessionID)
			}
		}
	}

	return sessionIDs
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

// EntireSessionID generates the full Entire session ID from a Claude session ID.
// The format is: YYYY-MM-DD-<claude-session-id>
func EntireSessionID(claudeSessionID string) string {
	return time.Now().Format("2006-01-02") + "-" + claudeSessionID
}

// ModelSessionID extracts the Claude session ID from an Entire session ID.
// The Entire session ID format is: YYYY-MM-DD-<claude-session-id>
// Returns the original string if it doesn't match the expected format.
func ModelSessionID(entireSessionID string) string {
	// Expected format: YYYY-MM-DD-<model-session-id> (11 chars prefix: "2025-12-02-")
	if len(entireSessionID) > 11 && entireSessionID[4] == '-' && entireSessionID[7] == '-' && entireSessionID[10] == '-' {
		return entireSessionID[11:]
	}
	// Return as-is if not in expected format (backwards compatibility)
	return entireSessionID
}

// SessionMetadataDir returns the path to a session's metadata directory.
// Takes a raw Claude session ID and adds the date prefix automatically.
func SessionMetadataDir(claudeSessionID string) string {
	return EntireMetadataDir + "/" + EntireSessionID(claudeSessionID)
}

// SessionMetadataDirFromEntireID returns the path to a session's metadata directory.
// Takes an Entire session ID (already date-prefixed) without adding another prefix.
func SessionMetadataDirFromEntireID(entireSessionID string) string {
	return EntireMetadataDir + "/" + entireSessionID
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

	// Write README if it doesn't exist (explains what this directory is for)
	readmePath := filepath.Join(entireDirAbs, ReadmeFileName)
	if _, statErr := os.Stat(readmePath); os.IsNotExist(statErr) {
		// Best-effort: don't fail if README can't be written
		_ = os.WriteFile(readmePath, []byte(EntireDirReadme), 0o644) //nolint:errcheck,gosec // best-effort, README should be world-readable
	}

	// Write session ID to file (no newline, just the ID)
	if err := os.WriteFile(sessionFileAbs, []byte(sessionID), 0o600); err != nil {
		return fmt.Errorf("failed to write current session file: %w", err)
	}

	return nil
}
