package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// TrailListResponse is the response from entire-api's trail list endpoint.
type TrailListResponse struct {
	Trails     []TrailResource `json:"items"`
	Total      int             `json:"total_count"`
	NextCursor *string         `json:"next_cursor"`
}

// TrailResource represents a trail returned by entire-api. The backend uses
// snake_case and nullable branch fields. Branch is empty when the trail is
// currently unlinked; OriginalBranch separately preserves its last link.
type TrailResource struct {
	ID                 string                `json:"id,omitempty"`
	Number             int                   `json:"number,omitempty"`
	URL                string                `json:"url,omitempty"`
	Branch             string                `json:"branch"`
	OriginalBranch     string                `json:"original_branch,omitempty"`
	Base               string                `json:"base"`
	Title              string                `json:"title"`
	Body               string                `json:"body,omitempty"`
	Status             string                `json:"status"`
	ReadyForReview     *bool                 `json:"ready_for_review,omitempty"` // nil when the server omits it
	Phase              string                `json:"phase,omitempty"`
	Author             *trail.Author         `json:"author"`
	Labels             []string              `json:"labels,omitempty"`
	Priority           string                `json:"priority,omitempty"`
	Type               string                `json:"type,omitempty"`
	Reviewers          []trail.Reviewer      `json:"reviewers,omitempty"`
	RequestedReviewers []string              `json:"requested_reviewers,omitempty"`
	CreatedAt          time.Time             `json:"created_at"`
	UpdatedAt          time.Time             `json:"updated_at"`
	MergedAt           *time.Time            `json:"merged_at,omitempty"`
	CheckpointCount    int                   `json:"checkpoint_count,omitempty"`
	CommitsAhead       int                   `json:"commits_ahead,omitempty"`
	BodyDocument       *TrailBodyDocument    `json:"body_document,omitempty"`
	Parent             *TrailParentReference `json:"parent,omitempty"`
	// Mergeability is served on the detail resource only; list items omit it.
	// It stays raw so that every detail-route decode (approve, update, resume,
	// review-target resolution, ...) does not depend on the snapshot's shape;
	// only `trail show` reads it, through DecodeMergeability.
	Mergeability json.RawMessage `json:"mergeability,omitempty"`
	// Agents is detail-only too, and raw for the same reason: `trail finding`
	// passes it through to --json and reads it through DecodeAgents.
	Agents json.RawMessage `json:"agents,omitempty"`
	// FromDetail reports that the resource was decoded from the detail route
	// rather than a list page. It is the reliable marker: body_document can be
	// absent from a valid detail response.
	FromDetail bool `json:"-"`
}

type TrailBodyDocument struct {
	TextSnapshot string `json:"text_snapshot"`
}

// DecodeMergeability decodes the detail resource's mergeability snapshot. It
// returns nil, nil when the snapshot is absent or null.
func (r *TrailResource) DecodeMergeability() (*TrailMergeability, error) {
	if RawJSONAbsent(r.Mergeability) {
		return nil, nil //nolint:nilnil // nil, nil means "no snapshot served"
	}
	var m TrailMergeability
	if err := json.Unmarshal(r.Mergeability, &m); err != nil {
		return nil, fmt.Errorf("decode trail mergeability: %w", err)
	}
	return &m, nil
}

// RawJSONAbsent reports a detail-only field the read did not carry: missing
// or null.
func RawJSONAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// ToMetadata converts a TrailResource to display metadata.
func (r *TrailResource) ToMetadata() *trail.Metadata {
	m := &trail.Metadata{
		Number: r.Number, TrailID: trail.ID(r.ID), URL: r.URL,
		Branch: r.Branch, OriginalBranch: r.OriginalBranch, Base: r.Base, Title: r.Title, Body: r.Body,
		Status: trail.Status(r.Status), Phase: r.Phase, Author: r.Author,
		Labels: r.Labels, Type: trail.Type(r.Type),
		Priority: trail.Priority(r.Priority), Reviewers: r.Reviewers,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, MergedAt: r.MergedAt,
	}
	if m.Labels == nil {
		m.Labels = []string{}
	}
	if r.Parent != nil {
		m.ProjectTrailURL = strings.TrimSpace(r.Parent.URL)
	}
	return m
}

type TrailUpdateRequest struct {
	// ReadyForReview marks the change ready (true) or back to draft (false).
	// The server keeps it false on a repository without an approval gate, but
	// still applies the matching draft state to a backing pull request.
	ReadyForReview *bool `json:"ready_for_review,omitempty"`
	// Base retargets the change onto another base branch.
	Base *string `json:"base,omitempty"`
}

type TrailUpdateResponse struct {
	Trail TrailResource `json:"trail"`
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
	CommitSHA string    `json:"commit_sha,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func (a *TrailApproval) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID        string          `json:"id"`
		Author    json.RawMessage `json:"author"`
		Event     string          `json:"event"`
		Body      *string         `json:"body"`
		CommitSHA string          `json:"commit_sha"`
		CreatedAt time.Time       `json:"created_at"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode trail approval: %w", err)
	}
	a.ID, a.Event, a.CommitSHA, a.CreatedAt = wire.ID, wire.Event, wire.CommitSHA, wire.CreatedAt
	if wire.Body != nil {
		a.Body = *wire.Body
	}
	if RawJSONAbsent(wire.Author) {
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

// TrailMergeabilityResponse is the full mergeability snapshot the detail
// resource serves (TrailMergeability, as `trail show` reads it) plus the
// fields the merge command needs to decide on and explain a bypass and to say
// what the merge will do with the trail's branch.
type TrailMergeabilityResponse struct {
	TrailMergeability

	BypassPolicy     string `json:"bypass_policy"`
	BehindBy         int    `json:"behind_by"`
	ComparisonStatus string `json:"comparison_status"`
	// DeleteBranchOnMerge is the repo's default for the merge request's
	// deleteBranch. It is nil for a GitHub-backed repo, whose branch GitHub
	// manages, when readiness is unavailable, and from a server that predates
	// the setting.
	DeleteBranchOnMerge *bool `json:"delete_branch_on_merge"`
}

// TrailMergeDetail is the part of GET .../trails/{number} the merge command
// reads: the mergeability snapshot and the caller's merge actions, both from
// the same server read.
type TrailMergeDetail struct {
	Actions      *TrailActions              `json:"actions"`
	Mergeability *TrailMergeabilityResponse `json:"mergeability"`
}

// TrailActions is the detail's per-caller availability of each operation;
// only the merge operations are decoded. The server revalidates on submit.
type TrailActions struct {
	Merge           TrailActionAvailability `json:"merge"`
	MergeWithBypass TrailActionAvailability `json:"merge_with_bypass"`
}

// TrailActionAvailability.State is enabled, blocked, or unavailable; Reason
// says why when it is not enabled.
type TrailActionAvailability struct {
	State  string  `json:"state"`
	Reason *string `json:"reason"`
}

type TrailMergeRequest struct {
	ExpectedHeadSha string `json:"expectedHeadSha,omitempty"`
	Bypass          bool   `json:"bypass,omitempty"`
	// DeleteBranch overrides the repo's delete-branch-on-merge setting for a
	// native trail; nil leaves it to the setting. A pointer, because an
	// explicit false must reach the server.
	DeleteBranch *bool `json:"deleteBranch,omitempty"`
}

type TrailMergeResponse struct {
	OK bool `json:"ok"`
	// Empty when the merge fast-forwarded or the base was already up to date.
	MergeCommitSha string `json:"mergeCommitSha"`
	// BranchDeleted reports whether the server deleted the trail's branch
	// after the merge. It is always false for a GitHub-backed trail, whose
	// branch GitHub manages.
	BranchDeleted bool `json:"branchDeleted"`
	// BranchKeptReason says why a native trail's branch was kept; empty when
	// it was deleted and for a GitHub-backed trail.
	BranchKeptReason string `json:"branchKeptReason,omitempty"`
}
