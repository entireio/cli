package index

import (
	"time"
)

const CurrentIndexVersion = 1

type IndexHeader struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	RepoRoot  string    `json:"repo_root"`
}

type PromptEntry struct {
	CheckpointID    string    `json:"checkpoint_id"`
	SessionIndex    int       `json:"session_index"`
	TurnIndex       int       `json:"turn_index"`
	Kind            string    `json:"kind"`
	PromptText      string    `json:"prompt_text"`
	PromptTruncated bool      `json:"prompt_truncated"`
	CommitHash      string    `json:"commit_hash"`
	CommitMessage   string    `json:"commit_message"`
	Branch          string    `json:"branch"`
	Agent           string    `json:"agent"`
	Model           string    `json:"model"`
	TokenCount      int       `json:"token_count"`
	ParentCheckpointID string `json:"parent_checkpoint_id,omitempty"`
	SubagentDepth   int       `json:"subagent_depth"`
	FilesTouched    []string  `json:"files_touched"`
	CreatedAt       time.Time `json:"created_at"`
}

type SearchConfig struct {
	Query   string
	Limit   int
	JSON    bool
	Agent   string
	Branch  string
	Kind    string
	After   string
	Files   string
}