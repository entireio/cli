package strategy

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

const (
	// logsOnlyScanLimit is the maximum number of commits to scan for logs-only points.
	logsOnlyScanLimit = 50

	// maxLastPromptRunes is the maximum rune length for LastPrompt stored in session state.
	maxLastPromptRunes = 100
)

// truncatePromptForStorage collapses whitespace and truncates a user prompt
// for storage in LastPrompt.
func truncatePromptForStorage(prompt string) string {
	return stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), maxLastPromptRunes, "...")
}

// SessionState is an alias for session.State.
// Previously this was a separate struct with manual conversion functions.
type SessionState = session.State

// PromptAttribution is an alias for session.PromptAttribution.
type PromptAttribution = session.PromptAttribution

// CheckpointInfo represents checkpoint metadata stored on the sessions branch.
// Metadata is stored at sharded path: <checkpoint_id[:2]>/<checkpoint_id[2:]>/
type CheckpointInfo struct {
	CheckpointID     id.CheckpointID `json:"checkpoint_id"` // 12-hex-char from Entire-Checkpoint trailer, used as directory path
	SessionID        string          `json:"session_id"`
	CreatedAt        time.Time       `json:"created_at"`
	CheckpointsCount int             `json:"checkpoints_count"`
	FilesTouched     []string        `json:"files_touched"`
	Agent            types.AgentType `json:"agent,omitempty"` // Human-readable agent name (e.g., "Claude Code")
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
	Prompts              []string // User prompts from the condensed session
	TotalTranscriptLines int      // Total lines in transcript after this condensation
	Transcript           []byte   // Raw transcript bytes for downstream consumers (trail title generation)
}

// ExtractedSessionData contains data extracted from a shadow branch.
type ExtractedSessionData struct {
	Transcript          []byte   // Full transcript content for the session
	FullTranscriptLines int      // Total line count in full transcript
	Prompts             []string // User prompts from the current checkpoint portion
	FilesTouched        []string
	TokenUsage          *agent.TokenUsage // Token usage calculated from transcript (since CheckpointTranscriptStart)
}
