package models

import (
	"time"
)

// Commit represents a Git commit in the repository history.
type Commit struct {
	SHA          string    `json:"sha"`
	ShortSHA     string    `json:"short_sha"`
	Message      string    `json:"message"`
	AuthorName   string    `json:"author_name"`
	AuthorEmail  string    `json:"author_email"`
	Timestamp    time.Time `json:"timestamp"`
	FilesChanged []string  `json:"files_changed"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
}

// CheckpointStatus indicates the availability and completeness of Entire Checkpoint context for a commit.
type CheckpointStatus string

const (
	CheckpointAvailable   CheckpointStatus = "AVAILABLE"
	CheckpointIncomplete  CheckpointStatus = "INCOMPLETE"
	CheckpointUnavailable CheckpointStatus = "UNAVAILABLE"
)

// CommitDevelopmentContext maps a Git commit to its corresponding Entire Checkpoint context.
type CommitDevelopmentContext struct {
	Commit               Commit           `json:"commit"`
	CheckpointStatus     CheckpointStatus `json:"checkpoint_status"`
	Checkpoint           *Checkpoint      `json:"checkpoint,omitempty"`
	HasCheckpoint        bool             `json:"has_checkpoint"`
	MissingContextReason string           `json:"missing_context_reason,omitempty"`
	Source               string           `json:"source"` // "git_and_checkpoint" or "git_only"
}
