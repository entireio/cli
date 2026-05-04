package api

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// TrailListResponse is the response from GET /api/v1/trails/:org/:repo.
type TrailListResponse struct {
	Trails        []TrailResource `json:"trails"`
	RepoFullName  string          `json:"repo_full_name"`
	DefaultBranch string          `json:"default_branch"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// TrailResource represents a single trail from the API.
type TrailResource struct {
	TrailID         string           `json:"trail_id"`
	Branch          string           `json:"branch"`
	Base            string           `json:"base"`
	Title           string           `json:"title"`
	Body            string           `json:"body"`
	Status          string           `json:"status"`
	Author          *trail.Author    `json:"author"`
	Assignees       []string         `json:"assignees"`
	Labels          []string         `json:"labels"`
	Priority        string           `json:"priority,omitempty"`
	Type            string           `json:"type,omitempty"`
	Reviewers       []trail.Reviewer `json:"reviewers,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	MergedAt        *time.Time       `json:"merged_at,omitempty"`
	CommentCount    int              `json:"comment_count,omitempty"`
	UnresolvedCount int              `json:"unresolved_count,omitempty"`
	CheckpointCount int              `json:"checkpoint_count,omitempty"`
	CommitsAhead    int              `json:"commits_ahead,omitempty"`
}

// ToMetadata converts a TrailResource to a trail.Metadata for display.
func (r *TrailResource) ToMetadata() *trail.Metadata {
	m := &trail.Metadata{
		TrailID:   trail.ID(r.TrailID),
		Branch:    r.Branch,
		Base:      r.Base,
		Title:     r.Title,
		Body:      r.Body,
		Status:    trail.Status(r.Status),
		Author:    r.Author,
		Assignees: r.Assignees,
		Labels:    r.Labels,
		Priority:  trail.Priority(r.Priority),
		Type:      trail.Type(r.Type),
		Reviewers: r.Reviewers,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
		MergedAt:  r.MergedAt,
	}
	if m.Assignees == nil {
		m.Assignees = []string{}
	}
	if m.Labels == nil {
		m.Labels = []string{}
	}
	return m
}

// TrailCreateRequest is the body for POST /api/v1/trails/:host/:owner/:repo.
type TrailCreateRequest struct {
	Title      string   `json:"title"`
	Body       string   `json:"body,omitempty"`
	BranchName string   `json:"branch_name"`
	Base       string   `json:"base,omitempty"`
	Status     string   `json:"status,omitempty"`
	Assignees  []string `json:"assignees,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Priority   string   `json:"priority,omitempty"`
	Type       string   `json:"type,omitempty"`
}

// TrailCreateResponse is the response from POST /api/v1/trails/:org/:repo.
type TrailCreateResponse struct {
	Trail         TrailResource `json:"trail"`
	BranchCreated bool          `json:"branch_created"`
}

// TrailDetailResponse is the response from GET /api/v1/trails/:org/:repo/:trailId.
type TrailDetailResponse struct {
	Trail       TrailResource     `json:"trail"`
	Discussion  trail.Discussion  `json:"discussion"`
	Checkpoints trail.Checkpoints `json:"checkpoints"`
}

// TrailUpdateRequest is the body for PATCH /api/v1/trails/:host/:owner/:repo/:trailId.
// Pointer fields distinguish "not provided" (nil) from "set to value".
// For slices, *[]string is used so nil means "no change" while &[]string{} means "clear".
type TrailUpdateRequest struct {
	Branch    *string   `json:"branch,omitempty"`
	Base      *string   `json:"base,omitempty"`
	Status    *string   `json:"status,omitempty"`
	Title     *string   `json:"title,omitempty"`
	Body      *string   `json:"body,omitempty"`
	Assignees *[]string `json:"assignees,omitempty"`
	Labels    *[]string `json:"labels,omitempty"`
	Priority  *string   `json:"priority,omitempty"`
	Type      *string   `json:"type,omitempty"`
}

// TrailUpdateResponse is the response from PATCH /api/v1/trails/:org/:repo/:trailId.
type TrailUpdateResponse struct {
	Trail TrailResource `json:"trail"`
}
