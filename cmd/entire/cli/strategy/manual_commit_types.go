package strategy

import "time"

const (
	// sessionStateDirName is the directory name for session state files within git common dir.
	sessionStateDirName = "entire-sessions"

	// logsOnlyScanLimit is the maximum number of commits to scan for logs-only points.
	logsOnlyScanLimit = 50
)

// SessionState represents the state of an active session.
type SessionState struct {
	SessionID                string    `json:"session_id"`
	BaseCommit               string    `json:"base_commit"`
	WorktreePath             string    `json:"worktree_path,omitempty"` // Absolute path to the worktree root
	StartedAt                time.Time `json:"started_at"`
	CheckpointCount          int       `json:"checkpoint_count"`
	CondensedTranscriptLines int       `json:"condensed_transcript_lines,omitempty"` // Lines already included in previous condensation
	UntrackedFilesAtStart    []string  `json:"untracked_files_at_start,omitempty"`   // Files that existed at session start (to preserve during rewind)
	FilesTouched             []string  `json:"files_touched,omitempty"`              // Files modified/created/deleted during this session
	ConcurrentWarningShown   bool      `json:"concurrent_warning_shown,omitempty"`   // True if user was warned about concurrent sessions
	LastCheckpointID         string    `json:"last_checkpoint_id,omitempty"`         // Checkpoint ID from last condensation, reused for subsequent commits without new content
	AgentType                string    `json:"agent_type,omitempty"`                 // Agent type identifier (e.g., "Claude Code", "Cursor")
}

// CheckpointInfo represents checkpoint metadata stored on the sessions branch.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
type CheckpointInfo struct {
	CheckpointID string    `json:"checkpoint_id"` // 12-hex-char from Entire-Checkpoint trailer, used as directory path
	SessionID    string    `json:"session_id"`
	CreatedAt    time.Time `json:"created_at"`
	StepsCount   int       `json:"steps_count,omitempty"` // Number of steps (prompt->response cycles) that led to this checkpoint
	FilesTouched []string  `json:"files_touched"`
	Agent        string    `json:"agent,omitempty"` // Human-readable agent name (e.g., "Claude Code")
	IsTask       bool      `json:"is_task,omitempty"`
	ToolUseID    string    `json:"tool_use_id,omitempty"`
	SessionCount int       `json:"session_count,omitempty"` // Number of sessions (1 if omitted)
	SessionIDs   []string  `json:"session_ids,omitempty"`   // All session IDs in this checkpoint
	// Deprecated: kept for backwards compatibility when reading old metadata
	CheckpointsCount int `json:"checkpoints_count,omitempty"`
}

// GetStepsCount returns steps count, falling back to CheckpointsCount for backwards compat.
func (c *CheckpointInfo) GetStepsCount() int {
	if c.StepsCount > 0 {
		return c.StepsCount
	}
	return c.CheckpointsCount
}

// CondenseResult contains the result of a session condensation operation.
type CondenseResult struct {
	CheckpointID         string // 12-hex-char from Entire-Checkpoint trailer, used as directory path
	SessionID            string
	StepsCount           int
	FilesTouched         []string
	TotalTranscriptLines int // Total lines in transcript after this condensation
}

// ExtractedSessionData contains data extracted from a shadow branch.
type ExtractedSessionData struct {
	Transcript          []byte   // Transcript content (lines after startLine for incremental extraction)
	FullTranscriptLines int      // Total line count in full transcript
	Prompts             []string // All user prompts from this portion
	Context             []byte   // Generated context.md content
	FilesTouched        []string
}
