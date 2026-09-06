// Package agentcheck contains the AgentCheck context boundary.
package agentcheck

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Context is the stable input that future AgentCheck evaluators consume.
// It intentionally contains AgentCheck-owned evidence shapes instead of
// checkpoint storage structs.
type Context struct {
	CheckpointID id.CheckpointID `json:"checkpoint_id"`
	Checkpoint   Checkpoint      `json:"checkpoint"`
	Sessions     []Session       `json:"sessions"`

	DeveloperPrompt string   `json:"developer_prompt,omitempty"`
	ScopedPrompts   []Prompt `json:"scoped_prompts,omitempty"`

	AgentType  string            `json:"agent_type,omitempty"`
	Model      string            `json:"model,omitempty"`
	TokenUsage *types.TokenUsage `json:"token_usage,omitempty"`

	FilesTouched []string     `json:"files_touched,omitempty"`
	ChangedFiles []FileChange `json:"changed_files,omitempty"`

	Git         GitEvidence   `json:"git"`
	Transcript  TranscriptRef `json:"transcript,omitempty"`
	TaskRecords []TaskRecord  `json:"task_records,omitempty"`
	Graph       GraphContext  `json:"graph"`

	Provenance Provenance `json:"provenance"`
}

// Checkpoint captures checkpoint-level facts needed by AgentCheck.
type Checkpoint struct {
	ID               id.CheckpointID `json:"id"`
	Strategy         string          `json:"strategy,omitempty"`
	Branch           string          `json:"branch,omitempty"`
	CommitSHA        string          `json:"commit_sha,omitempty"`
	CheckpointsCount int             `json:"checkpoints_count,omitempty"`
	SessionCount     int             `json:"session_count"`
	Imported         bool            `json:"imported,omitempty"`
	HasReview        bool            `json:"has_review,omitempty"`
	HasInvestigation bool            `json:"has_investigation,omitempty"`
}

// Session captures one Entire session's context inside a checkpoint.
type Session struct {
	Index                       int               `json:"index"`
	SessionID                   string            `json:"session_id,omitempty"`
	CreatedAt                   time.Time         `json:"created_at,omitempty"`
	Strategy                    string            `json:"strategy,omitempty"`
	Branch                      string            `json:"branch,omitempty"`
	CommitSHA                   string            `json:"commit_sha,omitempty"`
	CheckpointsCount            int               `json:"checkpoints_count,omitempty"`
	SaveStepCount               int               `json:"save_step_count,omitempty"`
	FilesTouched                []string          `json:"files_touched,omitempty"`
	AgentType                   string            `json:"agent_type,omitempty"`
	Model                       string            `json:"model,omitempty"`
	TurnID                      string            `json:"turn_id,omitempty"`
	Kind                        string            `json:"kind,omitempty"`
	IsTask                      bool              `json:"is_task,omitempty"`
	ToolUseID                   string            `json:"tool_use_id,omitempty"`
	TokenUsage                  *types.TokenUsage `json:"token_usage,omitempty"`
	TranscriptStart             int               `json:"transcript_start,omitempty"`
	CompactTranscriptStart      *int              `json:"compact_transcript_start,omitempty"`
	TranscriptUnavailable       bool              `json:"transcript_unavailable,omitempty"`
	TranscriptUnavailableReason string            `json:"transcript_unavailable_reason,omitempty"`
	PromptCount                 int               `json:"prompt_count,omitempty"`
	Prompts                     []Prompt          `json:"prompts,omitempty"`
	SkillEventCount             int               `json:"skill_event_count,omitempty"`
}

// Prompt preserves raw prompt text as stored in Entire checkpoint prompt data.
type Prompt struct {
	SessionIndex int    `json:"session_index"`
	PromptIndex  int    `json:"prompt_index"`
	Text         string `json:"text"`
}

// FileChange is a changed path associated with checkpoint commits.
type FileChange struct {
	Path   string `json:"path"`
	Status string `json:"status,omitempty"`
}

// AssociatedCommit describes a git commit linked to a checkpoint.
type AssociatedCommit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject,omitempty"`
	Author  string    `json:"author,omitempty"`
	Date    time.Time `json:"date,omitempty"`
	Source  string    `json:"source"`
}

// GitEvidence is the checkpoint-associated git evidence AgentCheck saw.
type GitEvidence struct {
	AssociatedCommits       []AssociatedCommit `json:"associated_commits,omitempty"`
	ChangedFiles            []FileChange       `json:"changed_files,omitempty"`
	Diff                    string             `json:"diff,omitempty"`
	DiffUnavailableReason   string             `json:"diff_unavailable_reason,omitempty"`
	ChangedFilesUnavailable string             `json:"changed_files_unavailable_reason,omitempty"`
}

// TranscriptRef records whether transcript bytes were available without making
// downstream evaluators understand checkpoint storage paths.
type TranscriptRef struct {
	Available         bool   `json:"available"`
	SessionID         string `json:"session_id,omitempty"`
	SessionIndex      int    `json:"session_index,omitempty"`
	ByteLength        int    `json:"byte_length,omitempty"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

// TaskRecord is reserved for durable subagent/task records when a stable reader
// surface is available. It remains empty rather than fabricated.
type TaskRecord struct {
	ToolUseID string `json:"tool_use_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

// GraphContext records optional graph evidence. Unavailable Graph never blocks
// context construction.
type GraphContext struct {
	Available         bool            `json:"available"`
	UnavailableReason string          `json:"unavailable_reason,omitempty"`
	Evidence          []GraphEvidence `json:"evidence,omitempty"`
}

// GraphEvidence is an adapter-owned shape for structural evidence supplied by
// Entire Graph or a test double.
type GraphEvidence struct {
	Query  string   `json:"query,omitempty"`
	Symbol string   `json:"symbol,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Paths  []string `json:"paths,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// Provenance carries the identifiers needed to trace future findings back to
// checkpoint, session, git, transcript, and graph evidence.
type Provenance struct {
	CheckpointID id.CheckpointID  `json:"checkpoint_id"`
	SessionIDs   []string         `json:"session_ids,omitempty"`
	CommitSHAs   []string         `json:"commit_shas,omitempty"`
	Sources      []EvidenceSource `json:"sources,omitempty"`
}

// EvidenceSource identifies where one class of context evidence came from.
type EvidenceSource struct {
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	Description string `json:"description,omitempty"`
	Available   bool   `json:"available"`
}
