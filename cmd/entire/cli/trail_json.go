package cli

import (
	"encoding/json"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// These CLI output types preserve the established JSON keys independently of
// the cell wire schema. Keep API structs at the HTTP boundary.

// mapSlice converts each element, keeping a nil input nil so the output
// distinguishes "absent" from "empty" exactly as the wire value did.
func mapSlice[T, U any](in []T, convert func(T) U) []U {
	if in == nil {
		return nil
	}
	out := make([]U, len(in))
	for i := range in {
		out[i] = convert(in[i])
	}
	return out
}

type trailResourceJSON struct {
	ID                 string                 `json:"id,omitempty"`
	Number             int                    `json:"number,omitempty"`
	URL                string                 `json:"url,omitempty"`
	Branch             string                 `json:"branch"`
	OriginalBranch     string                 `json:"originalBranch,omitempty"`
	Base               string                 `json:"base"`
	Title              string                 `json:"title"`
	Body               string                 `json:"body,omitempty"`
	Status             string                 `json:"status"`
	Phase              string                 `json:"phase,omitempty"`
	Author             *trail.Author          `json:"author"`
	Assignees          []string               `json:"assignees"`
	Labels             []string               `json:"labels,omitempty"`
	Priority           string                 `json:"priority,omitempty"`
	Type               string                 `json:"type,omitempty"`
	Reviewers          []trail.Reviewer       `json:"reviewers,omitempty"`
	RequestedReviewers []string               `json:"requestedReviewers,omitempty"`
	CreatedAt          time.Time              `json:"createdAt"`
	UpdatedAt          time.Time              `json:"updatedAt"`
	MergedAt           *time.Time             `json:"mergedAt,omitempty"`
	CommentCount       int                    `json:"commentCount,omitempty"`
	UnresolvedCount    int                    `json:"unresolvedCount,omitempty"`
	CheckpointCount    int                    `json:"checkpointCount,omitempty"`
	CommitsAhead       int                    `json:"commitsAhead,omitempty"`
	BodyDocument       *trailBodyDocumentJSON `json:"bodyDocument,omitempty"`
}

func toTrailResourceJSON(v api.TrailResource) trailResourceJSON {
	out := trailResourceJSON{
		ID:                 v.ID,
		Number:             v.Number,
		URL:                v.URL,
		Branch:             v.Branch,
		OriginalBranch:     v.OriginalBranch,
		Base:               v.Base,
		Title:              v.Title,
		Body:               v.Body,
		Status:             v.Status,
		Phase:              v.Phase,
		Author:             v.Author,
		Assignees:          v.Assignees,
		Labels:             v.Labels,
		Priority:           v.Priority,
		Type:               v.Type,
		Reviewers:          v.Reviewers,
		RequestedReviewers: v.RequestedReviewers,
		CreatedAt:          v.CreatedAt,
		UpdatedAt:          v.UpdatedAt,
		MergedAt:           v.MergedAt,
		CommentCount:       v.CommentCount,
		UnresolvedCount:    v.UnresolvedCount,
		CheckpointCount:    v.CheckpointCount,
		CommitsAhead:       v.CommitsAhead,
	}
	if v.BodyDocument != nil {
		doc := trailBodyDocumentJSON(*v.BodyDocument)
		out.BodyDocument = &doc
	}
	return out
}

type trailBodyDocumentJSON struct {
	TextSnapshot string `json:"textSnapshot"`
	ETag         string `json:"etag,omitempty"`
}

type trailApprovalsResponseJSON struct {
	Approvals []trailApprovalJSON `json:"approvals"`
}

func toTrailApprovalsResponseJSON(v api.TrailApprovalsResponse) trailApprovalsResponseJSON {
	return trailApprovalsResponseJSON{Approvals: mapSlice(v.Approvals, toTrailApprovalJSON)}
}

type trailApprovalJSON struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Event     string    `json:"event"`
	Body      string    `json:"body,omitempty"`
	CommitSHA string    `json:"commitSha,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func toTrailApprovalJSON(v api.TrailApproval) trailApprovalJSON { return trailApprovalJSON(v) }

type trailDiscussionsJSON struct {
	Items []trailDiscussionSummaryJSON `json:"items"`
}

func toTrailDiscussionsJSON(items []api.TrailDiscussionSummary) trailDiscussionsJSON {
	return trailDiscussionsJSON{Items: mapSlice(items, toTrailDiscussionSummaryJSON)}
}

type trailDiscussionSummaryJSON struct {
	ID                string                           `json:"id"`
	TrailID           string                           `json:"trailId"`
	Kind              string                           `json:"kind"`
	Title             string                           `json:"title"`
	ReviewCommentID   *string                          `json:"reviewCommentId"`
	Resolved          bool                             `json:"resolved"`
	ResolvedBy        *string                          `json:"resolvedBy"`
	ResolvedAt        *time.Time                       `json:"resolvedAt"`
	CreatedBy         *string                          `json:"createdBy"`
	CreatedAt         time.Time                        `json:"createdAt"`
	UpdatedAt         time.Time                        `json:"updatedAt"`
	LastMessageAt     *time.Time                       `json:"lastMessageAt"`
	LastMessageAuthor *string                          `json:"lastMessageAuthor"`
	MessageCount      int                              `json:"messageCount"`
	Participants      []trailDiscussionParticipantJSON `json:"participants"`
}

func toTrailDiscussionSummaryJSON(v api.TrailDiscussionSummary) trailDiscussionSummaryJSON {
	return trailDiscussionSummaryJSON{
		ID:                v.ID,
		TrailID:           v.TrailID,
		Kind:              v.Kind,
		Title:             v.Title,
		ReviewCommentID:   v.ReviewCommentID,
		Resolved:          v.Resolved,
		ResolvedBy:        v.ResolvedBy,
		ResolvedAt:        v.ResolvedAt,
		CreatedBy:         v.CreatedBy,
		CreatedAt:         v.CreatedAt,
		UpdatedAt:         v.UpdatedAt,
		LastMessageAt:     v.LastMessageAt,
		LastMessageAuthor: v.LastMessageAuthor,
		MessageCount:      v.MessageCount,
		Participants:      mapSlice(v.Participants, toTrailDiscussionParticipantJSON),
	}
}

type trailDiscussionParticipantJSON struct {
	Login string `json:"login"`
}

func toTrailDiscussionParticipantJSON(v api.TrailDiscussionParticipant) trailDiscussionParticipantJSON {
	return trailDiscussionParticipantJSON(v)
}

type trailDiscussionDetailResponseJSON struct {
	Discussion  trailDiscussionSummaryJSON   `json:"discussion"`
	Messages    []trailDiscussionMessageJSON `json:"messages"`
	EventCursor string                       `json:"eventCursor"`
}

func toTrailDiscussionDetailResponseJSON(v api.TrailDiscussionDetailResponse) trailDiscussionDetailResponseJSON {
	return trailDiscussionDetailResponseJSON{
		Discussion:  toTrailDiscussionSummaryJSON(v.Discussion),
		Messages:    mapSlice(v.Messages, toTrailDiscussionMessageJSON),
		EventCursor: v.EventCursor,
	}
}

type trailDiscussionMessageJSON struct {
	ID        string                     `json:"id"`
	Author    string                     `json:"author"`
	CreatedAt time.Time                  `json:"createdAt"`
	Body      string                     `json:"body"`
	Replies   []trailDiscussionReplyJSON `json:"replies"`
}

func toTrailDiscussionMessageJSON(v api.TrailDiscussionMessage) trailDiscussionMessageJSON {
	return trailDiscussionMessageJSON{
		ID:        v.ID,
		Author:    v.Author,
		CreatedAt: v.CreatedAt,
		Body:      v.Body,
		Replies:   mapSlice(v.Replies, toTrailDiscussionReplyJSON),
	}
}

type trailDiscussionReplyJSON struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	Body      string    `json:"body"`
}

func toTrailDiscussionReplyJSON(v api.TrailDiscussionReply) trailDiscussionReplyJSON {
	return trailDiscussionReplyJSON(v)
}

type trailDiscussionCreateResponseJSON struct {
	Discussion trailDiscussionSummaryJSON  `json:"discussion"`
	Message    *trailDiscussionMessageJSON `json:"message"`
}

func toTrailDiscussionCreateResponseJSON(v api.TrailDiscussionCreateResponse) trailDiscussionCreateResponseJSON {
	out := trailDiscussionCreateResponseJSON{Discussion: toTrailDiscussionSummaryJSON(v.Discussion)}
	if v.Message != nil {
		message := toTrailDiscussionMessageJSON(*v.Message)
		out.Message = &message
	}
	return out
}

// trailShowJSON is the `trail show --json` object: the same trail fields as one
// entry of `trail list --json`, plus the detail-only mergeability snapshot.
// Mergeability is always present as a key and null when the detail could not
// be loaded, so a missing verdict never reads as "mergeable": false.
type trailShowJSON struct {
	*trail.Metadata

	Mergeability *trailMergeabilityJSON `json:"mergeability"`
}

type trailMergeabilityJSON struct {
	HeadSHA        *string         `json:"head_sha"`
	Mergeable      bool            `json:"mergeable"`
	ConflictStatus string          `json:"conflict_status"`
	Checks         trailChecksJSON `json:"checks"`
	Gates          []trailGateJSON `json:"gates"`
}

func toTrailMergeabilityJSON(v *api.TrailMergeability) *trailMergeabilityJSON {
	if v == nil {
		return nil
	}
	return &trailMergeabilityJSON{
		HeadSHA:        v.HeadSHA,
		Mergeable:      v.Mergeable,
		ConflictStatus: v.ConflictStatus,
		Checks: trailChecksJSON{
			Availability: v.Checks.Availability,
			Runs:         mapSlice(v.Checks.Runs, toTrailCheckRunJSON),
		},
		Gates: mapSlice(v.Gates, toTrailGateJSON),
	}
}

type trailChecksJSON struct {
	Availability string              `json:"availability"`
	Runs         []trailCheckRunJSON `json:"runs"`
}

type trailCheckRunJSON struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  *string    `json:"conclusion"`
	DetailsURL  *string    `json:"details_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
	AppName     *string    `json:"app_name"`
}

func toTrailCheckRunJSON(v api.TrailCheckRun) trailCheckRunJSON { return trailCheckRunJSON(v) }

type trailGateJSON struct {
	ID               string                  `json:"id"`
	GateDefinitionID string                  `json:"gate_definition_id"`
	GateKey          string                  `json:"gate_key"`
	GateType         string                  `json:"gate_type"`
	Blocking         bool                    `json:"blocking"`
	Status           string                  `json:"status"`
	State            string                  `json:"state,omitempty"`
	Outcome          *string                 `json:"outcome"`
	Rationale        *string                 `json:"rationale"`
	HeadSHA          *string                 `json:"head_sha"`
	EvaluatedAtSHA   *string                 `json:"evaluated_at_sha"`
	StaleReason      *string                 `json:"stale_reason"`
	FindingCount     *int64                  `json:"finding_count"`
	Reviewers        []trailGateReviewerJSON `json:"reviewers"`
	RunnerIDs        []string                `json:"runner_ids"`
	Value            json.RawMessage         `json:"value"`
	CreatedAt        time.Time               `json:"created_at"`
	CompletedAt      *time.Time              `json:"completed_at"`
}

func toTrailGateJSON(v api.TrailGate) trailGateJSON {
	return trailGateJSON{
		ID:               v.ID,
		GateDefinitionID: v.GateDefinitionID,
		GateKey:          v.GateKey,
		GateType:         v.GateType,
		Blocking:         v.Blocking,
		Status:           v.Status,
		State:            v.State,
		Outcome:          v.Outcome,
		Rationale:        v.Rationale,
		HeadSHA:          v.HeadSHA,
		EvaluatedAtSHA:   v.EvaluatedAtSHA,
		StaleReason:      v.StaleReason,
		FindingCount:     v.FindingCount,
		Reviewers:        mapSlice(v.Reviewers, toTrailGateReviewerJSON),
		RunnerIDs:        v.RunnerIDs,
		Value:            v.Value,
		CreatedAt:        v.CreatedAt,
		CompletedAt:      v.CompletedAt,
	}
}

type trailGateReviewerJSON struct {
	Login           string  `json:"login"`
	State           string  `json:"state"`
	ReviewedHeadSHA *string `json:"reviewed_head_sha"`
	Reason          *string `json:"reason"`
}

func toTrailGateReviewerJSON(v api.TrailGateReviewer) trailGateReviewerJSON {
	return trailGateReviewerJSON(v)
}
