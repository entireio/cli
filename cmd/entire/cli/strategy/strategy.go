// Package strategy provides the manual-commit strategy: it tracks agent
// sessions in session state and condenses them into checkpoints when the user
// commits.
package strategy

import (
	"errors"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// ErrNoMetadata is returned when a commit does not have an Entire metadata trailer.
var ErrNoMetadata = errors.New("commit has no entire metadata")

// ErrEmptyRepository is returned when the repository has no commits yet.
var ErrEmptyRepository = errors.New("repository has no commits yet")

// PendingCheckpoint is one row of `checkpoint list --pending`, which is the
// resume view of the current branch rather than a single kind of thing. A row is
// one of two shapes:
//
//   - A subagent task record of a session based on HEAD, live or completed but
//     not yet condensed onto entire/checkpoints/v1. IsTaskCheckpoint is true,
//     ToolUseID names the task, and ID and CheckpointID are empty: nothing is
//     committed yet. (Turn-end steps live only in session state and have no
//     row.)
//   - A logs-only resume point: a commit on the current branch whose
//     Entire-Checkpoint trailer resolves to a checkpoint that IS already
//     condensed onto entire/checkpoints/v1. ID is that commit, CheckpointID the
//     condensed checkpoint, and IsLogsOnly is true. It is listed so the session
//     transcript can be restored from v1 (RestoreLogsOnly); file state would
//     need a git checkout.
//
// So "pending" describes the listing, not a guarantee that the underlying work
// is un-condensed — half these rows are recovered from condensed history.
// This abstraction allows different strategies to use different
// identifiers (commit hashes, branch names, stash refs, etc.)
type PendingCheckpoint struct {
	// ID is the unique identifier for this pending checkpoint
	// (commit hash, branch name, stash ref, etc.)
	ID string

	// Message is the human-readable description/summary
	Message string

	// MetadataDir is the path to the metadata directory
	MetadataDir string

	// Date is when this pending checkpoint was created
	Date time.Time

	// IsTaskCheckpoint indicates if this is a task checkpoint (vs a session checkpoint)
	IsTaskCheckpoint bool

	// ToolUseID is the tool use ID for task checkpoints (empty for session checkpoints)
	ToolUseID string

	// IsLogsOnly indicates this is a commit with condensed session logs.
	// The logs can be restored from entire/checkpoints/v1, but file state requires git checkout.
	IsLogsOnly bool

	// CheckpointID is the stable 12-hex-char identifier for logs-only points.
	// Used to retrieve logs from entire/checkpoints/v1/<id[:2]>/<id[2:]>/full.jsonl
	// Empty for task-record rows (uncommitted).
	CheckpointID id.CheckpointID

	// Agent is the human-readable name of the agent that created this checkpoint
	// (e.g., "Claude Code", "Cursor")
	Agent types.AgentType

	// SessionID is the session identifier for this checkpoint.
	// Used to distinguish checkpoints from different concurrent sessions.
	SessionID string

	// SessionPrompt is the initial prompt that started this session.
	// Used to help users identify which session a checkpoint belongs to.
	SessionPrompt string

	// SessionCount is the number of sessions in this checkpoint (1 for single-session).
	// Only populated for logs-only points with multi-session checkpoints.
	SessionCount int

	// SessionIDs contains all session IDs when this is a multi-session checkpoint.
	// The last entry is the most recent session (same as SessionID).
	// Only populated for logs-only points with multi-session checkpoints.
	SessionIDs []string

	// SessionPrompts contains the first prompt for each session (parallel to SessionIDs).
	// Used to display context when showing resume commands for multi-session checkpoints.
	SessionPrompts []string

	// Imported indicates this point is a read-only imported (commit-less)
	// checkpoint on the v1 metadata branch. Imported points are read-only.
	Imported bool
}

// StepContext contains all information needed for saving a step checkpoint.
// All file paths should be pre-filtered and normalized by the CLI layer.
type StepContext struct {
	// SessionID is the Claude Code session identifier
	SessionID string

	// ModifiedFiles is the list of files modified during the session
	// (extracted from the transcript, already filtered and relative)
	ModifiedFiles []string

	// NewFiles is the list of new files created during the session
	// (pre-computed by CLI from pre-prompt state comparison)
	NewFiles []string

	// DeletedFiles is the list of files deleted during the session
	// (tracked files that no longer exist)
	DeletedFiles []string

	// MetadataDir is the repo-relative path to the session metadata directory
	// (.entire/metadata/<session>). It is both the git tree path the directory's
	// files land at and the name the .entire root reads them through; see
	// checkpoint.WriteOptions.MetadataDir for why there is no absolute twin.
	MetadataDir string

	// CommitMessage is the generated commit message
	CommitMessage string

	// TranscriptPath is the path to the transcript file
	TranscriptPath string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// AgentType is the human-readable agent name (e.g., "Claude Code", "Cursor")
	AgentType types.AgentType

	// Transcript position at step/turn start - tracks what was added during this step
	StepTranscriptIdentifier string // Last identifier when step started (e.g., message UUID for Claude Code)
	StepTranscriptStart      int    // Transcript line count when this step/turn started

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *agent.TokenUsage

	// SubagentLedgerVersion is the authoritative inventory version observed
	// while token evidence was extracted. nil means no inventory snapshot;
	// a pointer to zero is a valid snapshot before the first child is observed.
	SubagentLedgerVersion *uint64
}

// TaskMetadataDir returns the path to a task's metadata directory
// within the session metadata directory.
func TaskMetadataDir(sessionMetadataDir, toolUseID string) string {
	return sessionMetadataDir + "/tasks/" + toolUseID
}

// RestoredSession describes a single session that was restored by RestoreLogsOnly.
// Each session may come from a different agent, so callers use this to print
// per-session resume commands without re-reading the metadata tree.
type RestoredSession struct {
	SessionID    string
	CheckpointID string
	Agent        types.AgentType
	Prompt       string
	CreatedAt    time.Time // From session metadata; used by resume to determine most recent
	Kind         string
	ReviewPrompt string
}
