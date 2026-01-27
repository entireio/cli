package strategy

import (
	"time"

	"entire.io/cli/cmd/entire/cli/agent"
	"entire.io/cli/cmd/entire/cli/checkpoint/id"
)

const (
	// sessionStateDirName is the directory name for session state files within git common dir.
	sessionStateDirName = "entire-sessions"

	// logsOnlyScanLimit is the maximum number of commits to scan for logs-only points.
	logsOnlyScanLimit = 50
)

// SessionState represents the state of an active session.
type SessionState struct {
	SessionID                string          `json:"session_id"`
	BaseCommit               string          `json:"base_commit"`
	WorktreePath             string          `json:"worktree_path,omitempty"` // Absolute path to the worktree root
	StartedAt                time.Time       `json:"started_at"`
	CheckpointCount          int             `json:"checkpoint_count"`
	CondensedTranscriptLines int             `json:"condensed_transcript_lines,omitempty"` // Lines already included in previous condensation
	UntrackedFilesAtStart    []string        `json:"untracked_files_at_start,omitempty"`   // Files that existed at session start (to preserve during rewind)
	FilesTouched             []string        `json:"files_touched,omitempty"`              // Files modified/created/deleted during this session
	ConcurrentWarningShown   bool            `json:"concurrent_warning_shown,omitempty"`   // True if user was warned about concurrent sessions
	LastCheckpointID         id.CheckpointID `json:"last_checkpoint_id,omitempty"`         // Checkpoint ID from last condensation, reused for subsequent commits without new content
	AgentType                agent.AgentType `json:"agent_type,omitempty"`                 // Agent type identifier (e.g., "Claude Code", "Cursor")
	TranscriptPath           string          `json:"transcript_path,omitempty"`            // Path to the live transcript file (for mid-session commit detection)

	// Token usage tracking (accumulated across all checkpoints in this session)
	TokenUsage *agent.TokenUsage `json:"token_usage,omitempty"`

	// Transcript position when session started (for multi-session checkpoints on entire/sessions)
	TranscriptLinesAtStart int    `json:"transcript_lines_at_start,omitempty"`
	TranscriptUUIDAtStart  string `json:"transcript_uuid_at_start,omitempty"`

	// PromptAttributions tracks user and agent line changes at each prompt start.
	PromptAttributions []PromptAttribution `json:"prompt_attributions,omitempty"`

	// PendingPromptAttribution holds attribution calculated at prompt start (before agent runs).
	// This is moved to PromptAttributions when SaveChanges is called.
	PendingPromptAttribution *PromptAttribution `json:"pending_prompt_attribution,omitempty"`
}

// PromptAttribution captures line-level attribution data at the start of each prompt,
// calculated BEFORE the agent runs. This allows us to separate user edits (made between
// prompts) from agent edits (made during prompt execution).
//
// Fields:
//   - CheckpointNumber: Which checkpoint this attribution is for (1-indexed)
//   - UserLinesAdded/Removed: User edits since the last checkpoint (lastCheckpoint → worktree)
//   - AgentLinesAdded/Removed: Cumulative agent work so far (base → lastCheckpoint)
//   - UserAddedPerFile: Per-file breakdown of user additions (for accurate modification tracking)
//
// Note: For checkpoint 1, AgentLinesAdded/Removed are always 0 because there is no previous
// checkpoint to compare against. This doesn't mean the agent hasn't done work yet - it means
// we can't measure cumulative agent work until after the first checkpoint is created.
// These fields become meaningful starting from checkpoint 2.
//
// UserAddedPerFile enables accurate tracking of user self-modifications. When a user modifies
// their own previously-added lines (not agent lines), we shouldn't subtract from the agent's
// contribution. See docs/architecture/attribution.md for details.
type PromptAttribution struct {
	CheckpointNumber  int            `json:"checkpoint_number"`
	UserLinesAdded    int            `json:"user_lines_added"`
	UserLinesRemoved  int            `json:"user_lines_removed"`
	AgentLinesAdded   int            `json:"agent_lines_added"`             // Always 0 for checkpoint 1 (no previous checkpoint)
	AgentLinesRemoved int            `json:"agent_lines_removed"`           // Always 0 for checkpoint 1 (no previous checkpoint)
	UserAddedPerFile  map[string]int `json:"user_added_per_file,omitempty"` // Per-file user additions for modification tracking
}

// CheckpointInfo represents checkpoint metadata stored on the sessions branch.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
type CheckpointInfo struct {
	CheckpointID     id.CheckpointID `json:"checkpoint_id"` // 12-hex-char from Entire-Checkpoint trailer, used as directory path
	SessionID        string          `json:"session_id"`
	CreatedAt        time.Time       `json:"created_at"`
	CheckpointsCount int             `json:"checkpoints_count"`
	FilesTouched     []string        `json:"files_touched"`
	Agent            agent.AgentType `json:"agent,omitempty"` // Human-readable agent name (e.g., "Claude Code")
	IsTask           bool            `json:"is_task,omitempty"`
	ToolUseID        string          `json:"tool_use_id,omitempty"`
	SessionCount     int             `json:"session_count,omitempty"` // Number of sessions (1 if omitted)
	SessionIDs       []string        `json:"session_ids,omitempty"`   // All session IDs in this checkpoint
}

// CondenseResult contains the result of a session condensation operation.
type CondenseResult struct {
	CheckpointID         id.CheckpointID // 12-hex-char from Entire-Checkpoint trailer, used as directory path
	SessionID            string
	CheckpointsCount     int
	FilesTouched         []string
	TotalTranscriptLines int // Total lines in transcript after this condensation
}

// ExtractedSessionData contains data extracted from a shadow branch.
type ExtractedSessionData struct {
	Transcript          []byte   // Full transcript content for the session
	FullTranscriptLines int      // Total line count in full transcript
	Prompts             []string // All user prompts from this portion
	Context             []byte   // Generated context.md content
	FilesTouched        []string
	TokenUsage          *agent.TokenUsage // Token usage calculated from transcript (since TranscriptLinesAtStart)
}
