package audit

import (
	"time"
)

// RiskSeverity represents the criticality of an identified risk.
type RiskSeverity string

const (
	SeverityHigh   RiskSeverity = "HIGH"
	SeverityMedium RiskSeverity = "MEDIUM"
	SeverityLow    RiskSeverity = "LOW"
)

// RiskCategory categorizes the type of risk identified in the checkpoint history.
type RiskCategory string

const (
	RiskCategoryToolFailure   RiskCategory = "tool_failure"
	RiskCategoryPendingTodo   RiskCategory = "pending_todo"
	RiskCategoryUntestedCode  RiskCategory = "untested_code"
	RiskCategoryUnfulfilled   RiskCategory = "unfulfilled_intent"
	RiskCategoryUnresolvedErr RiskCategory = "unresolved_error"
)

// IntentStatus represents whether a prompt goal was satisfied by the implementation.
type IntentStatus string

const (
	IntentStatusFulfilled IntentStatus = "FULFILLED"
	IntentStatusPartial   IntentStatus = "PARTIAL"
	IntentStatusMissing   IntentStatus = "MISSING"
)

// IntentItem represents a user prompt/requirement extracted from checkpoints.
type IntentItem struct {
	ID           string       `json:"id"`
	Prompt       string       `json:"prompt"`
	Timestamp    time.Time    `json:"timestamp"`
	Status       IntentStatus `json:"status"`
	FilesTouched []string     `json:"files_touched,omitempty"`
	Reasoning    string       `json:"reasoning,omitempty"`
}

// RiskItem represents a risk or unfinished requirement found in the audit.
type RiskItem struct {
	ID          string       `json:"id"`
	Severity    RiskSeverity `json:"severity"`
	Category    RiskCategory `json:"category"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	Location    string       `json:"location,omitempty"`
	Evidence    string       `json:"evidence,omitempty"`
}

// HandoffSummary holds context needed for another agent or developer to resume work seamlessly.
type HandoffSummary struct {
	Goal                 string   `json:"goal"`
	CompletedMilestones   []string `json:"completed_milestones"`
	AttemptedFailures    []string `json:"attempted_failures"`
	UnresolvedRisks      []string `json:"unresolved_risks"`
	NextRecommendedSteps []string `json:"next_recommended_steps"`
}

// AuditResult holds the overall audit findings, scores, and handoff data.
type AuditResult struct {
	BranchName       string         `json:"branch_name"`
	HeadCommit       string         `json:"head_commit"`
	CheckpointsCount int            `json:"checkpoints_count"`
	ReadinessScore   int            `json:"readiness_score"` // 0 to 100
	ReadinessGrade   string         `json:"readiness_grade"` // A, B, C, D, F
	Intents          []IntentItem   `json:"intents"`
	Risks            []RiskItem     `json:"risks"`
	Handoff          HandoffSummary `json:"handoff"`
	GraphEvidence    []string       `json:"graph_evidence,omitempty"`
	EvaluatedAt      time.Time      `json:"evaluated_at"`
}
