// Package trailers provides parsing and formatting for Entire commit message trailers.
// Trailers are key-value metadata appended to git commit messages following the
// git trailer convention (key: value format after a blank line).
package trailers

import (
	"fmt"
	"regexp"
	"strings"

	checkpointID "entire.io/cli/cmd/entire/cli/checkpoint/id"
)

// Trailer key constants used in commit messages.
const (
	// MetadataTrailerKey points to the metadata directory within a commit tree.
	MetadataTrailerKey = "Entire-Metadata"

	// MetadataTaskTrailerKey points to the task metadata directory for subagent checkpoints.
	MetadataTaskTrailerKey = "Entire-Metadata-Task"

	// StrategyTrailerKey indicates which strategy created the commit.
	StrategyTrailerKey = "Entire-Strategy"

	// BaseCommitTrailerKey links shadow commits to their base code commit.
	BaseCommitTrailerKey = "Base-Commit"

	// SessionTrailerKey identifies which session created a commit.
	SessionTrailerKey = "Entire-Session"

	// CondensationTrailerKey identifies the condensation ID for a commit (legacy).
	CondensationTrailerKey = "Entire-Condensation"

	// SourceRefTrailerKey links code commits to their metadata on a shadow/metadata branch.
	// Format: "<branch>@<commit-hash>" e.g. "entire/metadata@abc123def456"
	SourceRefTrailerKey = "Entire-Source-Ref"

	// CheckpointTrailerKey links commits to their checkpoint metadata on entire/sessions.
	// Format: 12 hex characters e.g. "a3b2c4d5e6f7"
	// This trailer survives git amend and rebase operations.
	CheckpointTrailerKey = "Entire-Checkpoint"

	// EphemeralBranchTrailerKey identifies the shadow branch that a checkpoint originated from.
	// Used in manual-commit strategy checkpoint commits on entire/sessions branch.
	// Format: full branch name e.g. "entire/2b4c177"
	EphemeralBranchTrailerKey = "Ephemeral-branch"

	// AgentTrailerKey identifies the agent that created a checkpoint.
	// Format: human-readable agent name e.g. "Claude Code", "Cursor"
	AgentTrailerKey = "Entire-Agent"
)

// Pre-compiled regexes for trailer parsing.
var (
	// Trailer parsing regexes.
	strategyTrailerRegex     = regexp.MustCompile(StrategyTrailerKey + `:\s*(.+)`)
	metadataTrailerRegex     = regexp.MustCompile(MetadataTrailerKey + `:\s*(.+)`)
	taskMetadataTrailerRegex = regexp.MustCompile(MetadataTaskTrailerKey + `:\s*(.+)`)
	baseCommitTrailerRegex   = regexp.MustCompile(BaseCommitTrailerKey + `:\s*([a-f0-9]{40})`)
	condensationTrailerRegex = regexp.MustCompile(CondensationTrailerKey + `:\s*(.+)`)
	sessionTrailerRegex      = regexp.MustCompile(SessionTrailerKey + `:\s*(.+)`)
	checkpointTrailerRegex   = regexp.MustCompile(CheckpointTrailerKey + `:\s*(` + checkpointID.Pattern + `)(?:\s|$)`)
)

// ParseStrategy extracts strategy from commit message.
// Returns the strategy name and true if found, empty string and false otherwise.
func ParseStrategy(commitMessage string) (string, bool) {
	matches := strategyTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return NormalizeStrategyName(strings.TrimSpace(matches[1])), true
	}
	return "", false
}

// ParseMetadata extracts metadata dir from commit message.
// Returns the metadata directory and true if found, empty string and false otherwise.
func ParseMetadata(commitMessage string) (string, bool) {
	matches := metadataTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseTaskMetadata extracts task metadata dir from commit message.
// Returns the task metadata directory and true if found, empty string and false otherwise.
func ParseTaskMetadata(commitMessage string) (string, bool) {
	matches := taskMetadataTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseBaseCommit extracts the base commit SHA from a commit message.
// Returns the full SHA and true if found, empty string and false otherwise.
func ParseBaseCommit(commitMessage string) (string, bool) {
	matches := baseCommitTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return matches[1], true
	}
	return "", false
}

// ParseCondensation extracts the condensation ID from a commit message.
// Returns the condensation ID and true if found, empty string and false otherwise.
func ParseCondensation(commitMessage string) (string, bool) {
	matches := condensationTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseSession extracts the session ID from a commit message.
// Returns the session ID and true if found, empty string and false otherwise.
// Note: If multiple Entire-Session trailers exist, this returns only the first one.
// Use ParseAllSessions to get all session IDs.
func ParseSession(commitMessage string) (string, bool) {
	matches := sessionTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseCheckpoint extracts the checkpoint ID from a commit message.
// Returns the CheckpointID and true if found, empty ID and false otherwise.
func ParseCheckpoint(commitMessage string) (checkpointID.CheckpointID, bool) {
	matches := checkpointTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		idStr := strings.TrimSpace(matches[1])
		// Validate it's a proper checkpoint ID
		if cpID, err := checkpointID.NewCheckpointID(idStr); err == nil {
			return cpID, true
		}
	}
	return checkpointID.EmptyCheckpointID, false
}

// ParseAllSessions extracts all session IDs from a commit message.
// Returns a slice of session IDs (may be empty if none found).
// Duplicate session IDs are deduplicated while preserving order.
// This is useful for commits that may have multiple Entire-Session trailers.
func ParseAllSessions(commitMessage string) []string {
	matches := sessionTrailerRegex.FindAllStringSubmatch(commitMessage, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	sessionIDs := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			sessionID := strings.TrimSpace(match[1])
			if !seen[sessionID] {
				seen[sessionID] = true
				sessionIDs = append(sessionIDs, sessionID)
			}
		}
	}
	return sessionIDs
}

// NormalizeStrategyName maps legacy strategy names to current names.
// These are the strategy names used during initial development and are kept for backwards
// compatibility.
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

// FormatStrategy creates a commit message with just the strategy trailer.
func FormatStrategy(message, strategy string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, StrategyTrailerKey, strategy)
}

// FormatTaskMetadata creates a commit message with task metadata trailer.
func FormatTaskMetadata(message, taskMetadataDir string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, MetadataTaskTrailerKey, taskMetadataDir)
}

// FormatTaskMetadataWithStrategy creates a commit message with task metadata and strategy trailers.
func FormatTaskMetadataWithStrategy(message, taskMetadataDir, strategy string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n%s: %s\n", message, MetadataTaskTrailerKey, taskMetadataDir, StrategyTrailerKey, strategy)
}

// FormatSourceRef creates a formatted source ref string for the trailer.
// Format: "<branch>@<commit-hash-prefix>" (hash truncated to 12 chars)
func FormatSourceRef(branch, commitHash string) string {
	shortHash := commitHash
	if len(shortHash) > 12 {
		shortHash = shortHash[:12]
	}
	return fmt.Sprintf("%s@%s", branch, shortHash)
}

// FormatMetadata creates a commit message with metadata trailer.
func FormatMetadata(message, metadataDir string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, MetadataTrailerKey, metadataDir)
}

// FormatMetadataWithStrategy creates a commit message with metadata and strategy trailers.
func FormatMetadataWithStrategy(message, metadataDir, strategy string) string {
	return fmt.Sprintf("%s\n\n%s: %s\n%s: %s\n", message, MetadataTrailerKey, metadataDir, StrategyTrailerKey, strategy)
}

// FormatShadowCommit creates a commit message for manual-commit strategy checkpoints.
// Includes Entire-Metadata, Entire-Session, and Entire-Strategy trailers.
func FormatShadowCommit(message, metadataDir, sessionID string) string {
	var sb strings.Builder
	sb.WriteString(message)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("%s: %s\n", MetadataTrailerKey, metadataDir))
	sb.WriteString(fmt.Sprintf("%s: %s\n", SessionTrailerKey, sessionID))
	sb.WriteString(fmt.Sprintf("%s: %s\n", StrategyTrailerKey, "manual-commit"))
	return sb.String()
}

// FormatShadowTaskCommit creates a commit message for manual-commit task checkpoints.
// Includes Entire-Metadata-Task, Entire-Session, and Entire-Strategy trailers.
func FormatShadowTaskCommit(message, taskMetadataDir, sessionID string) string {
	var sb strings.Builder
	sb.WriteString(message)
	sb.WriteString("\n\n")
	sb.WriteString(fmt.Sprintf("%s: %s\n", MetadataTaskTrailerKey, taskMetadataDir))
	sb.WriteString(fmt.Sprintf("%s: %s\n", SessionTrailerKey, sessionID))
	sb.WriteString(fmt.Sprintf("%s: %s\n", StrategyTrailerKey, "manual-commit"))
	return sb.String()
}
