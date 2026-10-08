package cli

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

type trailReviewCommentJSON struct {
	ID                        string                           `json:"id"`
	TrailID                   string                           `json:"trailId"`
	RepositoryID              string                           `json:"repositoryId"`
	ReviewID                  string                           `json:"reviewId"`
	CodeVersionID             string                           `json:"codeVersionId"`
	ActorID                   string                           `json:"actorId"`
	Title                     *string                          `json:"title"`
	Body                      *string                          `json:"body"`
	Severity                  *string                          `json:"severity"`
	Confidence                *float64                         `json:"confidence"`
	Status                    string                           `json:"status"`
	StatusReason              *string                          `json:"statusReason"`
	StaleOutcome              string                           `json:"staleOutcome"`
	StaleCheckedAt            *time.Time                       `json:"staleCheckedAt"`
	StaleCheckedCodeVersionID *string                          `json:"staleCheckedCodeVersionId"`
	ClientID                  *string                          `json:"clientId"`
	ClientIDHash              *string                          `json:"clientIdHash"`
	CreatedAt                 time.Time                        `json:"createdAt"`
	UpdatedAt                 time.Time                        `json:"updatedAt"`
	Location                  trailReviewLocationJSON          `json:"location"`
	SuggestedChanges          []trailReviewSuggestedChangeJSON `json:"suggestedChanges,omitempty"`
	DiscussionID              *string                          `json:"discussionId,omitempty"`
	DiscussionMessageCount    int                              `json:"discussionMessageCount,omitempty"`
	OutgoingLinks             []trailReviewOutgoingLinkJSON    `json:"outgoingLinks,omitempty"`
}

func toTrailReviewCommentJSON(v api.TrailReviewComment) trailReviewCommentJSON {
	return trailReviewCommentJSON{
		ID:                        v.ID,
		TrailID:                   v.TrailID,
		RepositoryID:              v.RepositoryID,
		ReviewID:                  v.ReviewID,
		CodeVersionID:             v.CodeVersionID,
		ActorID:                   v.ActorID,
		Title:                     v.Title,
		Body:                      v.Body,
		Severity:                  v.Severity,
		Confidence:                v.Confidence,
		Status:                    v.Status,
		StatusReason:              v.StatusReason,
		StaleOutcome:              v.StaleOutcome,
		StaleCheckedAt:            v.StaleCheckedAt,
		StaleCheckedCodeVersionID: v.StaleCheckedCodeVersionID,
		ClientID:                  v.ClientID,
		ClientIDHash:              v.ClientIDHash,
		CreatedAt:                 v.CreatedAt,
		UpdatedAt:                 v.UpdatedAt,
		Location:                  trailReviewLocationJSON(v.Location),
		SuggestedChanges:          mapSlice(v.SuggestedChanges, toTrailReviewSuggestedChangeJSON),
		DiscussionID:              v.DiscussionID,
		DiscussionMessageCount:    v.DiscussionMessageCount,
		OutgoingLinks:             mapSlice(v.OutgoingLinks, toTrailReviewOutgoingLinkJSON),
	}
}

func toTrailReviewCommentsJSON(comments []api.TrailReviewComment) []trailReviewCommentJSON {
	return mapSlice(comments, toTrailReviewCommentJSON)
}

type trailReviewLocationJSON struct {
	ID              string  `json:"id"`
	ReviewCommentID string  `json:"reviewCommentId"`
	CodeVersionID   string  `json:"codeVersionId"`
	Granularity     string  `json:"granularity"`
	FilePath        *string `json:"filePath"`
	StartLine       *int    `json:"startLine"`
	StartColumn     *int    `json:"startColumn"`
	EndLine         *int    `json:"endLine"`
	EndColumn       *int    `json:"endColumn"`
	SelectedText    *string `json:"selectedText"`
	NearbyText      *string `json:"nearbyText"`
	Language        *string `json:"language"`
}

type trailReviewSuggestedChangeJSON struct {
	ID                string    `json:"id"`
	ReviewCommentID   string    `json:"reviewCommentId"`
	CodeVersionID     string    `json:"codeVersionId"`
	ChangeType        string    `json:"changeType"`
	Patch             *string   `json:"patch"`
	Instruction       *string   `json:"instruction"`
	ExpectedFilePath  *string   `json:"expectedFilePath"`
	ExpectedFileHash  *string   `json:"expectedFileHash"`
	ExpectedStartLine *int      `json:"expectedStartLine"`
	ExpectedEndLine   *int      `json:"expectedEndLine"`
	ExpectedLines     *string   `json:"expectedLines"`
	CreatedBy         string    `json:"createdBy"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

func toTrailReviewSuggestedChangeJSON(v api.TrailReviewSuggestedChange) trailReviewSuggestedChangeJSON {
	return trailReviewSuggestedChangeJSON(v)
}

type trailReviewOutgoingLinkJSON struct {
	SourceCommentID string `json:"sourceCommentId"`
	TargetCommentID string `json:"targetCommentId"`
	LinkType        string `json:"linkType"`
}

func toTrailReviewOutgoingLinkJSON(v api.TrailReviewOutgoingLink) trailReviewOutgoingLinkJSON {
	return trailReviewOutgoingLinkJSON(v)
}
