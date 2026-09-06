package models

import (
	"time"
)

// Checkpoint represents a preserved Entire checkpoint session tied to a commit.
type Checkpoint struct {
	CheckpointID           string    `json:"checkpoint_id"`
	CommitRef              string    `json:"commit_ref"`
	Timestamp              time.Time `json:"timestamp"`
	IntentContext          string    `json:"intent_context"`
	FilesChanged           []string  `json:"files_changed"`
	AssociatedRequirements []string  `json:"associated_requirements"`
	VerificationInfo       string    `json:"verification_info"`
}
