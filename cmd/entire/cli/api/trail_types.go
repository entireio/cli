package api

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// TrailListResponse is the response from entire-api's trail list endpoint.
// Trails is retained as the CLI-facing page field; UnmarshalJSON fills it from
// the backend's standard {items,nextPageToken,totalCount} envelope.
type TrailListResponse struct {
	Trails        []TrailResource `json:"items"`
	Total         int             `json:"totalCount"`
	NextPageToken *string         `json:"nextPageToken"`

	// Legacy metadata fields remain source-compatible for integration fixtures;
	// entire-api's standard list envelope no longer emits them.
	Limit         int       `json:"-"`
	Offset        int       `json:"-"`
	RepoFullName  string    `json:"-"`
	DefaultBranch string    `json:"-"`
	UpdatedAt     time.Time `json:"-"`
}

func (r *TrailListResponse) UnmarshalJSON(data []byte) error {
	var wire struct {
		Items         []TrailResource `json:"items"`
		Trails        []TrailResource `json:"trails"`
		TotalCount    int             `json:"totalCount"`
		Total         int             `json:"total"`
		NextPageToken *string         `json:"nextPageToken"`
	}
	if err := decodeNormalizedTrailJSON(data, &wire); err != nil {
		return fmt.Errorf("decode trail list: %w", err)
	}
	r.Trails = wire.Items
	if r.Trails == nil {
		r.Trails = wire.Trails
	}
	r.Total = wire.TotalCount
	if r.Total == 0 {
		r.Total = wire.Total
	}
	r.NextPageToken = wire.NextPageToken
	return nil
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

func (r *TrailResource) UnmarshalJSON(data []byte) error {
	type plain TrailResource
	return decodeNormalizedTrailJSON(data, (*plain)(r))
}

// TrailBodyDocument is the trail's description editor document. TextSnapshot
// is the rendered plain text displayed by the CLI.
type TrailBodyDocument struct {
	TextSnapshot string `json:"textSnapshot"`
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
type TrailUpdateRequest struct {
	Status             *string   `json:"status,omitempty"`
	Title              *string   `json:"title,omitempty"`
	Body               *string   `json:"body,omitempty"`
	Labels             *[]string `json:"labels,omitempty"`
	Assignees          *[]string `json:"assignees,omitempty"`
	RequestedReviewers *[]string `json:"requestedReviewers,omitempty"`
	Type               *string   `json:"type,omitempty"`
	Priority           *string   `json:"priority,omitempty"`
}

type TrailUpdateResponse struct {
	Trail TrailResource `json:"trail"`
}

// Kept for source compatibility with callers that model the former BFF body.
// entire-api now confirms deletion with a 204 No Content response.
type TrailDeleteResponse struct {
	OK bool `json:"ok"`
}

// TrailApproval is a single approval decision on a trail. Author is exposed as
// a login string while UnmarshalJSON accepts both the BFF's string and
// entire-api's {id,login} object.
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
	if err := decodeNormalizedTrailJSON(data, &wire); err != nil {
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
