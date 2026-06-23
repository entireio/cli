package api

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// TrailListResponse is the response from GET /api/v1/trails/:org/:repo.
// The endpoint paginates: Trails holds one page (server max 200 rows) and
// Total is the full match count for the requested filters.
type TrailListResponse struct {
	Trails        []TrailResource `json:"trails"`
	Total         int             `json:"total"`
	Limit         int             `json:"limit"`
	Offset        int             `json:"offset"`
	RepoFullName  string          `json:"repo_full_name"`
	DefaultBranch string          `json:"default_branch"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// TrailResource represents a single trail from the API.
type TrailResource struct {
	ID              string           `json:"id,omitempty"`
	Number          int              `json:"number,omitempty"`
	URL             string           `json:"url,omitempty"`
	Branch          string           `json:"branch"`
	Base            string           `json:"base"`
	Title           string           `json:"title"`
	Body            string           `json:"body"`
	Status          string           `json:"status"`
	Phase           string           `json:"phase,omitempty"`
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
	// BodyDocument carries the trail's description (collaborative editor doc).
	// The list endpoint omits it; the detail endpoint populates it.
	BodyDocument *TrailBodyDocument `json:"body_document,omitempty"`
}

// TrailBodyDocument is the trail's description editor document. TextSnapshot is
// the rendered plain text the CLI displays.
type TrailBodyDocument struct {
	TextSnapshot string `json:"text_snapshot"`
}

// ToMetadata converts a TrailResource to a trail.Metadata for display.
func (r *TrailResource) ToMetadata() *trail.Metadata {
	m := &trail.Metadata{
		Number:    r.Number,
		TrailID:   trail.ID(r.ID),
		URL:       r.URL,
		Branch:    r.Branch,
		Base:      r.Base,
		Title:     r.Title,
		Body:      r.Body,
		Status:    trail.Status(r.Status),
		Phase:     r.Phase,
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
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	BranchName string `json:"branch_name"`
	// BranchAction is "create" (default) or "link". The CLI sends "link" to
	// attach the already-pushed branch instead of backfilling it at base.
	BranchAction string   `json:"branch_action,omitempty"`
	Base         string   `json:"base,omitempty"`
	Status       string   `json:"status,omitempty"`
	Assignees    []string `json:"assignees,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Type         string   `json:"type,omitempty"`
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

// TrailDeleteResponse is the response from DELETE /api/v1/trails/:host/:owner/:repo/:number.
// OK is the server's explicit success signal; a destructive delete should not be
// reported as done unless it is true.
type TrailDeleteResponse struct {
	OK bool `json:"ok"`
}
