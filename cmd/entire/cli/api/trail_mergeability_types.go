package api

import (
	"encoding/json"
	"time"
)

// TrailMergeability is the backend's mergeability snapshot for a trail, served
// on the detail resource (TrailDetailResponse.mergeability). It is the
// authoritative merge verdict: the CLI surfaces it as-is and never derives
// mergeability from its parts. head_sha, gates, and checks all come from the
// same server read that decided mergeable, so they are consistent with it.
//
// The wire object carries further fields (approval_gate_passed, behind_by,
// comparison_status, bypass_policy) that the CLI does not expose.
type TrailMergeability struct {
	HeadSHA        *string     `json:"head_sha"`
	Mergeable      bool        `json:"mergeable"`
	ConflictStatus string      `json:"conflict_status"`
	Checks         TrailChecks `json:"checks"`
	Gates          []TrailGate `json:"gates"`
}

// TrailChecks.Availability values. Runs are meaningful only when available.
const (
	TrailChecksAvailable     = "available"
	TrailChecksUnavailable   = "unavailable"
	TrailChecksNotApplicable = "not_applicable"
)

// TrailChecks are the third-party CI runs for the snapshot's head.
type TrailChecks struct {
	Availability string          `json:"availability"`
	Runs         []TrailCheckRun `json:"runs"`
}

// TrailCheckRun is one CI run. Conclusion is null until the run completes.
type TrailCheckRun struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion"`
	DetailsURL  *string    `json:"details_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	AppName     *string    `json:"app_name"`
}

// TrailGate is one evaluated gate row. Value's shape depends on GateType
// (approvals, checks, findings, ...), so it is kept as raw JSON and passed
// through untouched.
type TrailGate struct {
	ID               string              `json:"id"`
	GateDefinitionID string              `json:"gate_definition_id"`
	GateKey          string              `json:"gate_key"`
	GateType         string              `json:"gate_type"`
	Blocking         bool                `json:"blocking"`
	Status           string              `json:"status"`
	State            string              `json:"state,omitempty"`
	Outcome          *string             `json:"outcome"`
	Rationale        *string             `json:"rationale"`
	HeadSHA          *string             `json:"head_sha"`
	EvaluatedAtSHA   *string             `json:"evaluated_at_sha"`
	StaleReason      *string             `json:"stale_reason"`
	FindingCount     *int64              `json:"finding_count"`
	Reviewers        []TrailGateReviewer `json:"reviewers"`
	RunnerIDs        []string            `json:"runner_ids"`
	Value            json.RawMessage     `json:"value"`
	CreatedAt        time.Time           `json:"created_at"`
	CompletedAt      *time.Time          `json:"completed_at"`
}

// TrailGateReviewer is a reviewer's standing on an approvals gate.
type TrailGateReviewer struct {
	Login           string  `json:"login"`
	State           string  `json:"state"`
	ReviewedHeadSHA *string `json:"reviewed_head_sha"`
	Reason          *string `json:"reason"`
}
