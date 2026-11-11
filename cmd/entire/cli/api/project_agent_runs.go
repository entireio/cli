package api

import (
	"encoding/json"
	"time"
)

// ProjectAgentRun is a project agent's run, as GET /{host}/{project}/runs and
// .../runs/{id} return it. Only the fields the CLI renders are modeled; the
// commands' --json prints the server's response unchanged.
type ProjectAgentRun struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    *string `json:"code"`
		Message string  `json:"message"`
	} `json:"error"`
	Agent struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Name    string `json:"name"`
	} `json:"agent"`
	Trigger struct {
		Kind string `json:"kind"`
	} `json:"trigger"`
	// TrailID, ChangeID and Branch are on list items; a single run's read
	// carries them in Target instead.
	TrailID  string `json:"trail_id"`
	ChangeID string `json:"change_id"`
	Branch   string `json:"branch"`
	Target   *struct {
		TrailID  string `json:"trail_id"`
		ChangeID string `json:"change_id"`
		Ref      string `json:"ref"`
		HeadSHA  string `json:"head_sha"`
	} `json:"target"`
	Attempt             int     `json:"attempt"`
	RetryOf             *string `json:"retry_of"`
	Diagnostic          *string `json:"diagnostic"`
	ReviewChangesResult *struct {
		Summary *string `json:"summary"`
		Signal  struct {
			ValueType string          `json:"value_type"`
			Value     json.RawMessage `json:"value"`
			Rationale string          `json:"rationale"`
		} `json:"signal"`
		Findings []json.RawMessage `json:"findings"`
	} `json:"review_changes_result"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProjectAgentRunPage is one page of GET /{host}/{project}/runs.
type ProjectAgentRunPage struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor *string           `json:"next_cursor,omitempty"`
}
