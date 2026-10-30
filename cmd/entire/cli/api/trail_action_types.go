package api

import (
	"encoding/json"
	"fmt"
	"time"
)

// TrailRunnerState is one configured runner's state on a trail detail read
// (TrailResource.Runners).
type TrailRunnerState struct {
	RunnerID       string     `json:"runner_id"`
	State          string     `json:"state"`
	Outcome        *string    `json:"outcome"`
	RunID          *string    `json:"run_id"`
	EvaluatedAtSHA *string    `json:"evaluated_at_sha"`
	StartedAt      *time.Time `json:"started_at"`
	StaleReason    *string    `json:"stale_reason"`
}

// DecodeRunners decodes the detail resource's runner states; nil when the
// read did not carry them.
func (r *TrailResource) DecodeRunners() ([]TrailRunnerState, error) {
	if rawJSONAbsent(r.Runners) {
		return nil, nil
	}
	var out []TrailRunnerState
	if err := json.Unmarshal(r.Runners, &out); err != nil {
		return nil, fmt.Errorf("decode trail runners: %w", err)
	}
	return out, nil
}

// TrailRunnerRun is a runner's latest run on a trail.
type TrailRunnerRun struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
	ErrorMessage *string `json:"errorMessage"`
	PromptStatus *string `json:"promptStatus"`
	HeadSHA      *string `json:"headSha"`
}

// TrailRunnerResult is the structured output a run reported.
type TrailRunnerResult struct {
	Type    string          `json:"type"`
	Adapter string          `json:"adapter"`
	Data    json.RawMessage `json:"data"`
}

// TrailRunnerLatestResponse is GET /repos/{repo_id}/trails/{number}/runs/{runnerId}.
// Run is nil when the runner never ran on this trail.
type TrailRunnerLatestResponse struct {
	Run    *TrailRunnerRun    `json:"run"`
	Result *TrailRunnerResult `json:"result"`
}

// TrailRunnerLaunchResponse is POST /repos/{repo_id}/trails/{number}/runs/{runnerId}.
// Outcome distinguishes a new launch from already_in_progress and
// already_complete, which start nothing.
type TrailRunnerLaunchResponse struct {
	RunID   string `json:"runId"`
	Outcome string `json:"outcome"`
}

// TrailReviewRerunResponse is POST /repos/{repo_id}/trails/{number}/review-rerun.
type TrailReviewRerunResponse struct {
	Launched int `json:"launched"`
}

// TrailUpdateBranchResponse is POST /repos/{repo_id}/trails/{number}/update-branch.
// MergeCommitSHA is empty when the branch was already up to date.
type TrailUpdateBranchResponse struct {
	MergeCommitSHA string `json:"mergeCommitSha"`
}

// TrailCIPullRequestResponse is POST .../trails/{number}/ci-pr.
type TrailCIPullRequestResponse struct {
	PRNumber int `json:"prNumber"`
}
