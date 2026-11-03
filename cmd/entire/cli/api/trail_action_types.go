package api

import (
	"encoding/json"
	"fmt"
)

// TrailAgent is one project agent on a change, as the change detail projects
// it (TrailResource.Agents): the agent, its latest reading and run, or what it
// waits for before its first run. Only the fields the CLI renders are
// modeled; --json passes the server's items through.
type TrailAgent struct {
	Agent struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"agent"`
	Reading    *TrailAgentReading `json:"reading"`
	TrendDelta *int               `json:"trend_delta"`
	LatestRun  *TrailAgentRun     `json:"latest_run"`
	WaitingFor *string            `json:"waiting_for"`
}

// TrailAgentReading is a finished review's reading; the field value_type
// names is the one set.
type TrailAgentReading struct {
	PercentValue *int    `json:"percent_value"`
	SizeValue    *string `json:"size_value"`
	BooleanValue *bool   `json:"boolean_value"`
	Summary      *string `json:"summary"`
	RunID        string  `json:"run_id"`
	HeadSHA      *string `json:"head_sha"`
}

// TrailAgentRun is an agent's latest run on a change.
type TrailAgentRun struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	HeadSHA   *string `json:"head_sha"`
	CreatedAt string  `json:"created_at"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// DecodeAgents decodes the detail resource's agents; nil when the read did
// not carry them.
func (r *TrailResource) DecodeAgents() ([]TrailAgent, error) {
	if rawJSONAbsent(r.Agents) {
		return nil, nil
	}
	var out []TrailAgent
	if err := json.Unmarshal(r.Agents, &out); err != nil {
		return nil, fmt.Errorf("decode trail agents: %w", err)
	}
	return out, nil
}

// AgentRunRequest is POST /{host}/{project}/agents/{id}/runs: one run of the
// agent on one change of a project trail.
type AgentRunRequest struct {
	Target AgentRunTarget `json:"target"`
}

// AgentRunTarget names the change a run reviews; the change's repository is
// the run's.
type AgentRunTarget struct {
	TrailID  string `json:"trail_id"`
	ChangeID string `json:"change_id"`
}

// AgentRun is the run an agent run request started, as far as the CLI reports
// it.
type AgentRun struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Agent  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"agent"`
}

// TrailReviewRerunResponse is POST /repos/{repo_id}/trails/{number}/review-rerun.
// Launched counts only the repository-configured reviewers it started; the
// project's agents are started the way a push starts them and are not counted.
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
