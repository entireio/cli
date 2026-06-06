// Package strategy provides the manual-commit strategy for managing
// Claude Code session changes via shadow branches and checkpoint condensation.
package strategy

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// ErrNoMetadata is returned when a commit does not have an Entire metadata trailer.
var ErrNoMetadata = errors.New("commit has no entire metadata")

// ErrNoSession is returned when no session info is available.
var ErrNoSession = errors.New("no session info available")

// ErrNotTaskCheckpoint is returned when a rewind point is not a task checkpoint.
var ErrNotTaskCheckpoint = errors.New("not a task checkpoint")

// ErrEmptyRepository is returned when the repository has no commits yet.
var ErrEmptyRepository = errors.New("repository has no commits yet")

// SessionInfo contains information about the current session state.
// This is used to generate trailers for linking commits to their AI session.
type SessionInfo struct {
	// SessionID is the session identifier extracted from the latest commit's metadata
	SessionID string

	// Reference is a strategy-specific reference string.
	// For manual-commit strategy: "entire/abc1234" (the shadow branch name)
	// Empty for commit strategy (metadata is in the same commit).
	Reference string

	// CommitHash is the full SHA of the commit containing the session metadata.
	// Empty for commit strategy.
	CommitHash string
}

// RewindPoint represents a point to which the user can rewind.
// This abstraction allows different strategies to use different
// identifiers (commit hashes, branch names, stash refs, etc.)
type RewindPoint struct {
	// ID is the unique identifier for this rewind point
	// (commit hash, branch name, stash ref, etc.)
	ID string

	// Message is the human-readable description/summary
	Message string

	// MetadataDir is the path to the metadata directory
	MetadataDir string

	// Date is when this rewind point was created
	Date time.Time

	// IsTaskCheckpoint indicates if this is a task checkpoint (vs a session checkpoint)
	IsTaskCheckpoint bool

	// ToolUseID is the tool use ID for task checkpoints (empty for session checkpoints)
	ToolUseID string

	// IsLogsOnly indicates this is a commit with session logs but no shadow branch state.
	// The logs can be restored from entire/checkpoints/v1, but file state requires git checkout.
	IsLogsOnly bool

	// CheckpointID is the stable 12-hex-char identifier for logs-only points.
	// Used to retrieve logs from entire/checkpoints/v1/<id[:2]>/<id[2:]>/full.jsonl
	// Empty for shadow branch checkpoints (uncommitted).
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
}

// RewindPreview describes what will happen when rewinding to a checkpoint.
// Used to warn users about files that will be modified or deleted.
type RewindPreview struct {
	// FilesToRestore are files from the checkpoint that will be written/restored.
	FilesToRestore []string

	// FilesToDelete are untracked files that will be removed.
	// These are files created after the checkpoint that aren't in the checkpoint tree
	// and weren't present at session start.
	FilesToDelete []string

	// TrackedChanges are tracked files with uncommitted changes that will be reverted.
	// These come from the existing CanRewind() warning.
	TrackedChanges []string
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

	// MetadataDir is the path to the session metadata directory
	MetadataDir string

	// MetadataDirAbs is the absolute path to the session metadata directory
	MetadataDirAbs string

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
	StepTranscriptIdentifier string // Last identifier when step started (UUID for Claude, message ID for Gemini)
	StepTranscriptStart      int    // Transcript line count when this step/turn started

	// TokenUsage contains the token usage for this checkpoint
	TokenUsage *agent.TokenUsage
}

// TaskStepContext contains all information needed for saving a task step checkpoint.
// This is called by the PostToolUse[Task] hook when a subagent completes.
// The strategy is responsible for creating metadata structures and storing them
// according to its storage approach.
type TaskStepContext struct {
	// SessionID is the Claude Code session identifier
	SessionID string

	// ToolUseID is the unique identifier for this Task tool invocation
	ToolUseID string

	// AgentID is the subagent identifier (from tool_response.agentId)
	AgentID string

	// ModifiedFiles is the list of files modified by the subagent
	// (extracted from the subagent's transcript)
	ModifiedFiles []string

	// NewFiles is the list of new files created by the subagent
	// (computed from pre-task state comparison)
	NewFiles []string

	// DeletedFiles is the list of files deleted by the subagent
	DeletedFiles []string

	// TranscriptPath is the path to the main session transcript
	TranscriptPath string

	// SubagentTranscriptPath is the path to the subagent's transcript (if available)
	SubagentTranscriptPath string

	// CheckpointUUID is the UUID for transcript truncation when rewinding
	CheckpointUUID string

	// AuthorName is the name to use for commits
	AuthorName string

	// AuthorEmail is the email to use for commits
	AuthorEmail string

	// IsIncremental indicates this is an incremental checkpoint during task execution,
	// not a final task completion checkpoint. When true:
	// - Writes to checkpoints/NNN-{tool-use-id}.json instead of checkpoint.json
	// - Skips transcript handling
	// - Uses incremental commit message
	IsIncremental bool

	// IncrementalSequence is the checkpoint sequence number (1, 2, 3, ...)
	// Only used when IsIncremental is true
	IncrementalSequence int

	// IncrementalType is the tool that triggered this checkpoint ("TodoWrite", "Edit", etc.)
	// Only used when IsIncremental is true
	IncrementalType string

	// IncrementalData is the tool_input payload for this checkpoint
	// Only used when IsIncremental is true
	IncrementalData json.RawMessage

	// SubagentType is the type of subagent (e.g., "dev", "reviewer")
	// Extracted from tool_input.subagent_type in Task tool
	// Used for descriptive commit messages
	SubagentType string

	// TaskDescription is the task description provided to the subagent
	// Extracted from tool_input.description in Task tool
	// Used for descriptive commit messages
	TaskDescription string

	// TodoContent is the content of the in-progress todo item
	// Extracted from tool_input.todos where status == "in_progress"
	// Used for descriptive incremental checkpoint messages
	TodoContent string

	// AgentType is the human-readable agent name (e.g., "Claude Code", "Cursor")
	AgentType types.AgentType
}

// TaskCheckpoint contains the checkpoint information written to checkpoint.json
type TaskCheckpoint struct {
	SessionID      string `json:"session_id"`
	ToolUseID      string `json:"tool_use_id"`
	CheckpointUUID string `json:"checkpoint_uuid"`
	AgentID        string `json:"agent_id,omitempty"`
}

// SubagentCheckpoint represents an intermediate checkpoint created during subagent execution.
// These are created by PostToolUse hooks for tools like TodoWrite, Edit, Write.
type SubagentCheckpoint struct {
	Type      string          `json:"type"`        // Tool name: "TodoWrite", "Edit", "Write"
	ToolUseID string          `json:"tool_use_id"` // The tool use ID that created this checkpoint
	Timestamp time.Time       `json:"timestamp"`   // When the checkpoint was created
	Data      json.RawMessage `json:"data"`        // Type-specific payload (tool_input)
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
	SessionID string
	Agent     types.AgentType
	Prompt    string
	CreatedAt time.Time // From session metadata; used by resume to determine most recent
}
