package strategy

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// NoDescription is the default description for sessions without one.
const NoDescription = "No description"

// Session represents a Claude Code session with its checkpoints.
// A session is created when a user runs `claude` and tracks all changes
// made during that interaction.
type Session struct {
	// ID is the unique session identifier (e.g., "2025-12-01-8f76b0e8-b8f1-4a87-9186-848bdd83d62e")
	ID string

	// Description is a human-readable summary of the session
	// (typically the first prompt or derived from commit messages)
	Description string

	// Strategy is the name of the strategy that created this session
	Strategy string

	// StartTime is when the session was started
	StartTime time.Time

	// Checkpoints is the list of save points within this session
	Checkpoints []Checkpoint
}

// Checkpoint represents a save point within a session.
// Checkpoints can be either session-level (on Stop) or task-level (on subagent completion).
type Checkpoint struct {
	// CheckpointID is the stable 12-hex-char identifier for this checkpoint.
	// Used to look up metadata at <id[:2]>/<id[2:]>/ on entire/checkpoints/v1 branch.
	CheckpointID id.CheckpointID

	// Message is the commit message or checkpoint description
	Message string

	// Timestamp is when this checkpoint was created
	Timestamp time.Time

	// IsTaskCheckpoint indicates if this is a task checkpoint (vs a session checkpoint)
	IsTaskCheckpoint bool

	// ToolUseID is the tool use ID for task checkpoints (empty for session checkpoints)
	ToolUseID string
}
