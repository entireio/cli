package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// TrailListResponse is the response from entire-api's trail list endpoint.
type TrailListResponse struct {
	Trails        []TrailResource `json:"items"`
	Total         int             `json:"totalCount"`
	NextPageToken *string         `json:"nextPageToken"`
}

// TrailResource represents a trail returned by entire-api. The backend uses
// camelCase and nullable branch fields. Branch is empty when the trail is
// currently unlinked; OriginalBranch separately preserves its last link.
type TrailResource struct {
	ID                 string             `json:"id,omitempty"`
	Number             int                `json:"number,omitempty"`
	URL                string             `json:"url,omitempty"`
	Branch             string             `json:"branch"`
	OriginalBranch     string             `json:"originalBranch,omitempty"`
	Base               string             `json:"base"`
	Title              string             `json:"title"`
	Body               string             `json:"body,omitempty"`
	Status             string             `json:"status"`
	Phase              string             `json:"phase,omitempty"`
	Author             *trail.Author      `json:"author"`
	Assignees          []string           `json:"assignees"`
	Labels             []string           `json:"labels,omitempty"`
	Priority           string             `json:"priority,omitempty"`
	Type               string             `json:"type,omitempty"`
	Reviewers          []trail.Reviewer   `json:"reviewers,omitempty"`
	RequestedReviewers []string           `json:"requestedReviewers,omitempty"`
	CreatedAt          time.Time          `json:"createdAt"`
	UpdatedAt          time.Time          `json:"updatedAt"`
	MergedAt           *time.Time         `json:"mergedAt,omitempty"`
	CommentCount       int                `json:"commentCount,omitempty"`
	UnresolvedCount    int                `json:"unresolvedCount,omitempty"`
	CheckpointCount    int                `json:"checkpointCount,omitempty"`
	CommitsAhead       int                `json:"commitsAhead,omitempty"`
	BodyDocument       *TrailBodyDocument `json:"bodyDocument,omitempty"`
}

// TrailBodyDocument is the trail's description editor document. TextSnapshot
// is the rendered plain text displayed by the CLI. The document is also what a
// body write returns (see TrailBodyRequest), so both directions decode into this
// type; the fields the CLI does not use (id, documentKey, schemaVersion,
// contentJson, updatedAt) are simply left out of it. ETag is populated on a
// read as well as on a write response, and is what makes If-Match viable on
// the next write (see sendTrailBody).
type TrailBodyDocument struct {
	TextSnapshot string `json:"textSnapshot"`
	ETag         string `json:"etag,omitempty"`
}

// ToMetadata converts a TrailResource to display metadata.
func (r *TrailResource) ToMetadata() *trail.Metadata {
	m := &trail.Metadata{
		Number: r.Number, TrailID: trail.ID(r.ID), URL: r.URL,
		Branch: r.Branch, Base: r.Base, Title: r.Title, Body: r.Body,
		Status: trail.Status(r.Status), Phase: r.Phase, Author: r.Author,
		Assignees: r.Assignees, Labels: r.Labels, Type: trail.Type(r.Type),
		Priority: trail.Priority(r.Priority), Reviewers: r.Reviewers,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, MergedAt: r.MergedAt,
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
	Title        string   `json:"title"`
	Body         string   `json:"body,omitempty"`
	BranchName   string   `json:"branchName,omitempty"`
	BranchAction string   `json:"branchAction,omitempty"`
	Base         string   `json:"base,omitempty"`
	Status       string   `json:"status,omitempty"`
	Assignees    []string `json:"assignees,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Type         string   `json:"type,omitempty"`
}

type TrailCreateResponse struct {
	Trail TrailResource `json:"trail"`
}

// TrailUpdateRequest uses pointers to distinguish absent fields from clears.
// There is deliberately no Labels field: the trails API does not accept label
// writes, so `trail update` exposes no label flags (labels are read-only, see
// TrailResource.Labels).
//
// There is deliberately no Body field either. The trails API does not serve
// body writes on this route — it rejects a body field outright and names the
// dedicated route to use instead — so the description has its own route and its
// own request shape; see TrailBodyRequest. Do not reintroduce the field to save
// a request: the rejection has been served as a redacted 5xx, which reads to
// the caller as a flaky server rather than as the wrong route, and that is what
// made this bug survive as long as it did.
type TrailUpdateRequest struct {
	Status             *string   `json:"status,omitempty"`
	Title              *string   `json:"title,omitempty"`
	Assignees          *[]string `json:"assignees,omitempty"`
	RequestedReviewers *[]string `json:"requestedReviewers,omitempty"`
	Type               *string   `json:"type,omitempty"`
	Priority           *string   `json:"priority,omitempty"`
}

type TrailUpdateResponse struct {
	Trail TrailResource `json:"trail"`
}

// TrailBodyRequest is the body for PUT
// /api/v1/trails/:host/:owner/:repo/:number/body, the only route that writes a
// trail's description (see TrailUpdateRequest for why it is not PATCH). The
// route answers with the resulting document, which decodes into
// TrailBodyDocument.
//
// Markdown carries no omitempty: an empty string is how a description is
// cleared, and the server distinguishes present-and-empty from absent — with
// omitempty the field would vanish from the JSON and the request would be
// rejected as "exactly one of markdown/contentJson is required".
//
// The route also accepts contentJson (ProseMirror JSON, written as-is) in place
// of markdown; the CLI only ever writes Markdown, so contentJson is not
// modeled here. The route also accepts an If-Match header for optimistic
// concurrency, populated from a prior read of TrailBodyDocument.ETag — see
// sendTrailBody for the dispatch between If-Match and Overwrite.
type TrailBodyRequest struct {
	Markdown  string `json:"markdown"`
	Overwrite bool   `json:"overwrite,omitempty"`
}

// TrailApproval is a single approval decision on a trail. Author is exposed as
// a login string while UnmarshalJSON accepts both shapes entire-api itself
// uses: the approvals collection sends a bare login string, the trail resource
// sends an {id,login} object. Both are live — this is not legacy tolerance.
type TrailApproval struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Event     string    `json:"event"`
	Body      string    `json:"body,omitempty"`
	CommitSHA string    `json:"commitSha,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func (a *TrailApproval) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID        string          `json:"id"`
		Author    json.RawMessage `json:"author"`
		Event     string          `json:"event"`
		Body      *string         `json:"body"`
		CommitSHA string          `json:"commitSha"`
		CreatedAt time.Time       `json:"createdAt"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode trail approval: %w", err)
	}
	a.ID, a.Event, a.CommitSHA, a.CreatedAt = wire.ID, wire.Event, wire.CommitSHA, wire.CreatedAt
	if wire.Body != nil {
		a.Body = *wire.Body
	}
	if len(wire.Author) == 0 || string(wire.Author) == "null" {
		return nil
	}
	if err := json.Unmarshal(wire.Author, &a.Author); err == nil {
		return nil
	}
	var author trail.Author
	if err := json.Unmarshal(wire.Author, &author); err != nil {
		return fmt.Errorf("decode trail approval author: %w", err)
	}
	if author.Login != nil {
		a.Author = *author.Login
	}
	return nil
}

type TrailApprovalRequest struct {
	Event string `json:"event"`
	Body  string `json:"body,omitempty"`
}

type TrailApprovalResponse struct {
	OK       bool          `json:"ok"`
	Approval TrailApproval `json:"approval"`
}

type TrailApprovalsResponse struct {
	Approvals []TrailApproval `json:"approvals"`
}
