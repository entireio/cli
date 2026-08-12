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
	ID        string           `json:"id,omitempty"`
	Number    int              `json:"number,omitempty"`
	URL       string           `json:"url,omitempty"`
	Branch    string           `json:"branch"`
	Base      string           `json:"base"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	Status    string           `json:"status"`
	Phase     string           `json:"phase,omitempty"`
	Author    *trail.Author    `json:"author"`
	Assignees []string         `json:"assignees"`
	Labels    []string         `json:"labels"`
	Priority  string           `json:"priority,omitempty"`
	Type      string           `json:"type,omitempty"`
	Reviewers []trail.Reviewer `json:"reviewers,omitempty"`
	// RequestedReviewers holds the logins requested for review (distinct from
	// Reviewers, which carries per-login review status). The replace-set
	// --add-reviewer/--remove-reviewer merge computes the new set from this.
	RequestedReviewers []string   `json:"requested_reviewers,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	MergedAt           *time.Time `json:"merged_at,omitempty"`
	CommentCount       int        `json:"comment_count,omitempty"`
	UnresolvedCount    int        `json:"unresolved_count,omitempty"`
	CheckpointCount    int        `json:"checkpoint_count,omitempty"`
	CommitsAhead       int        `json:"commits_ahead,omitempty"`
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
		Type:      trail.Type(r.Type),
		Priority:  trail.Priority(r.Priority),
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
	BranchName string `json:"branch_name,omitempty"`
	// BranchAction is "create" (default) or "link". The CLI sends "link" to
	// attach an already-pushed branch instead of backfilling it at base. Omit
	// both branch_name and branch_action to create a branchless trail.
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
	Trail TrailResource `json:"trail"`
}

// TrailUpdateRequest is the body for PATCH /api/v1/trails/:host/:owner/:repo/:trailId.
// Pointer fields distinguish "not provided" (nil) from "set to value".
// For slices, *[]string is used so nil means "no change" while &[]string{} means "clear".
type TrailUpdateRequest struct {
	Status             *string   `json:"status,omitempty"`
	Title              *string   `json:"title,omitempty"`
	Body               *string   `json:"body,omitempty"`
	Labels             *[]string `json:"labels,omitempty"`
	Assignees          *[]string `json:"assignees,omitempty"`
	RequestedReviewers *[]string `json:"requested_reviewers,omitempty"`
	Type               *string   `json:"type,omitempty"`
	Priority           *string   `json:"priority,omitempty"`
	// Branch changes the branch a trail that already has one points at. The
	// server writes it straight to the trail row (a raw metadata rename); it
	// does not create or verify the branch on the forge. To attach a branch to
	// a branchless trail (with forge create/link), use TrailBranchRequest
	// against the /branch endpoint instead.
	Branch *string `json:"branch,omitempty"`
}

// TrailUpdateResponse is the response from PATCH /api/v1/trails/:org/:repo/:trailId.
type TrailUpdateResponse struct {
	Trail TrailResource `json:"trail"`
}

// TrailBranchRequest is the body for POST /api/v1/trails/:host/:owner/:repo/:number/branch.
// It attaches a branch to a branchless trail; the server rejects it (409) when
// the trail already has a branch. Action is required: "create" backfills the
// branch on the forge at the trail's base if it is missing (needs App write
// permission); "link" attaches an already-pushed branch and never backfills it.
type TrailBranchRequest struct {
	Action     string `json:"action"`
	BranchName string `json:"branch_name,omitempty"`
}

// TrailBranchResponse is the response from POST /api/v1/trails/:host/:owner/:repo/:number/branch.
// BranchCreated reports whether the server created the branch on the forge
// (true for a "create" action that backfilled a missing branch).
type TrailBranchResponse struct {
	Trail         TrailResource `json:"trail"`
	BranchCreated bool          `json:"branch_created"`
}

// TrailDeleteResponse is the response from DELETE /api/v1/trails/:host/:owner/:repo/:number.
// OK is the server's explicit success signal; a destructive delete should not be
// reported as done unless it is true.
type TrailDeleteResponse struct {
	OK bool `json:"ok"`
}

// TrailApproval is a single approval decision on a trail.
//
// Author is a GitHub login string, not a *trail.Author object — the approvals
// endpoint sends `"author":"nodo"` where the trail resource sends
// `"author":{"id":…,"login":…}`. Matching TrailThread/TrailMessage, which use the
// same string shape. Declaring an object here made every populated response fail
// to decode, so `entire trail approvals` worked only while there was nothing to
// show (see TestTrailApprovalDecodesStringAuthor).
type TrailApproval struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"` // GitHub login
	Event     string    `json:"event"`  // "approved" | "changes_requested"
	Body      string    `json:"body,omitempty"`
	CommitSHA string    `json:"commit_sha,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TrailApprovalRequest is the body for POST .../:number/approvals.
// Event is "APPROVE" or "REQUEST_CHANGES"; Body is required for REQUEST_CHANGES.
type TrailApprovalRequest struct {
	Event string `json:"event"`
	Body  string `json:"body,omitempty"`
}

// TrailApprovalResponse is the response from POST .../:number/approvals.
type TrailApprovalResponse struct {
	OK       bool          `json:"ok"`
	Approval TrailApproval `json:"approval"`
}

// TrailApprovalsResponse is the response from GET .../:number/approvals.
type TrailApprovalsResponse struct {
	Approvals []TrailApproval `json:"approvals"`
}
