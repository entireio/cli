package cli

import (
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
