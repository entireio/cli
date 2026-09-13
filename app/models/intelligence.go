package models

import (
	"time"
)

// ContextCompleteness represents the completeness of the Entire Checkpoint context.
type ContextCompleteness string

const (
	ContextComplete   ContextCompleteness = "COMPLETE"
	ContextIncomplete ContextCompleteness = "INCOMPLETE"
	ContextRedacted   ContextCompleteness = "REDACTED"
	ContextUnavailable ContextCompleteness = "UNAVAILABLE"
)

// VerificationStatus represents the evidence-based verification confidence level.
type VerificationStatus string

const (
	VerificationCompleted         VerificationStatus = "COMPLETED"
	VerificationPartiallyVerified VerificationStatus = "PARTIALLY_VERIFIED"
	VerificationIncomplete        VerificationStatus = "INCOMPLETE"
	VerificationNeedsVerification VerificationStatus = "NEEDS_VERIFICATION"
)

// EvidenceItem represents proof or verification details from a single information source.
type EvidenceItem struct {
	Available bool   `json:"available"`
	Summary   string `json:"summary"`
	Details   string `json:"details,omitempty"`
}

// EvidenceMatrix aggregates proof across Checkpoint, Commit, Source, Tests, and Graph.
type EvidenceMatrix struct {
	Checkpoint EvidenceItem `json:"checkpoint"`
	Commit     EvidenceItem `json:"commit"`
	Source     EvidenceItem `json:"source"`
	Tests      EvidenceItem `json:"tests"`
	Graph      EvidenceItem `json:"graph"`
}

// CheckpointIntelligence represents the structured developer intelligence for a commit/checkpoint session.
type CheckpointIntelligence struct {
	CheckpointID        string              `json:"checkpoint_id"`
	CommitSHA           string              `json:"commit_sha"`
	ShortSHA            string              `json:"short_sha"`
	RequirementID       string              `json:"requirement_id,omitempty"`
	RequirementTitle    string              `json:"requirement_title,omitempty"`
	Intent              string              `json:"intent"`
	Implemented         []string            `json:"implemented"`
	Incomplete          []string            `json:"incomplete"`
	Evidence            EvidenceMatrix      `json:"evidence"`
	NextAction          string              `json:"next_action"`
	ContextCompleteness ContextCompleteness `json:"context_completeness"`
	VerificationStatus  VerificationStatus  `json:"verification_status"`
	GeneratedAt         time.Time           `json:"generated_at"`
}
