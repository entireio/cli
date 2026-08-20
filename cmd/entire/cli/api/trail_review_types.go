package api

import "time"

// TrailReviewStateResponse is returned by GET /api/v1/trails/{trail_id}/reviews/{id}.
type TrailReviewStateResponse struct {
	Review      TrailReview            `json:"review"`
	CodeVersion TrailReviewCodeVersion `json:"codeVersion"`
	Counts      TrailReviewCounts      `json:"counts"`
	Comments    []TrailReviewComment   `json:"comments"`
	NextCursor  *string                `json:"nextCursor"`
	EventCursor string                 `json:"eventCursor"`
}

// TrailReview represents a review session.
type TrailReview struct {
	ID            string    `json:"id"`
	TrailID       string    `json:"trailId"`
	CodeVersionID string    `json:"codeVersionId"`
	ActorID       string    `json:"actorId"`
	Summary       *string   `json:"summary"`
	StartedAt     time.Time `json:"startedAt"`
}

// TrailReviewCodeVersion pins the base/head that a review covers.
type TrailReviewCodeVersion struct {
	ID           string    `json:"id"`
	TrailID      string    `json:"trailId"`
	RepositoryID string    `json:"repositoryId"`
	BaseRef      *string   `json:"baseRef"`
	HeadRef      *string   `json:"headRef"`
	BaseSHA      *string   `json:"baseSha"`
	HeadSHA      *string   `json:"headSha"`
	CapturedAt   time.Time `json:"capturedAt"`
}

// TrailReviewCounts are review-scoped comment counts.
type TrailReviewCounts struct {
	Open      int `json:"open"`
	Resolved  int `json:"resolved"`
	Dismissed int `json:"dismissed"`
	Stale     int `json:"stale"`
	Total     int `json:"total"`
}

// TrailReviewCommentsResponse is returned by trail/review comment list endpoints.
type TrailReviewCommentsResponse struct {
	Comments    []TrailReviewComment `json:"comments"`
	HasMore     bool                 `json:"hasMore"`
	NextOffset  *int                 `json:"nextOffset"`
	EventCursor string               `json:"eventCursor,omitempty"`
}

// TrailReviewComment is a single agent-native review finding.
type TrailReviewComment struct {
	ID                        string                       `json:"id"`
	TrailID                   string                       `json:"trailId"`
	RepositoryID              string                       `json:"repositoryId"`
	ReviewID                  string                       `json:"reviewId"`
	CodeVersionID             string                       `json:"codeVersionId"`
	ActorID                   string                       `json:"actorId"`
	Title                     *string                      `json:"title"`
	Body                      *string                      `json:"body"`
	Severity                  *string                      `json:"severity"`
	Confidence                *float64                     `json:"confidence"`
	Status                    string                       `json:"status"`
	StatusReason              *string                      `json:"statusReason"`
	StaleOutcome              string                       `json:"staleOutcome"`
	StaleCheckedAt            *time.Time                   `json:"staleCheckedAt"`
	StaleCheckedCodeVersionID *string                      `json:"staleCheckedCodeVersionId"`
	ClientID                  *string                      `json:"clientId"`
	ClientIDHash              *string                      `json:"clientIdHash"`
	CreatedAt                 time.Time                    `json:"createdAt"`
	UpdatedAt                 time.Time                    `json:"updatedAt"`
	Location                  TrailReviewLocation          `json:"location"`
	SuggestedChanges          []TrailReviewSuggestedChange `json:"suggestedChanges,omitempty"`
	ThreadID                  *string                      `json:"threadId,omitempty"`
	ThreadMessageCount        int                          `json:"threadMessageCount,omitempty"`
	OutgoingLinks             []TrailReviewOutgoingLink    `json:"outgoingLinks,omitempty"`
}

// TrailReviewStartRequest starts a review session for a trail via
// POST /api/v1/trails/{trail_id}/reviews. All fields are optional; the server
// resolves the code version (base/head) when they are omitted.
type TrailReviewStartRequest struct {
	HeadSHA *string `json:"headSha,omitempty"`
	BaseSHA *string `json:"baseSha,omitempty"`
	BaseRef *string `json:"baseRef,omitempty"`
	HeadRef *string `json:"headRef,omitempty"`
}

// TrailReviewStartResponse is returned by POST /api/v1/trails/{trail_id}/reviews.
type TrailReviewStartResponse struct {
	ReviewID       string            `json:"reviewId"`
	TrailID        string            `json:"trailId"`
	RepositoryID   string            `json:"repositoryId"`
	CodeVersionID  string            `json:"codeVersionId"`
	BaseSHA        *string           `json:"baseSha"`
	HeadSHA        *string           `json:"headSha"`
	EventStreamURL string            `json:"eventStreamUrl"`
	DiffURL        string            `json:"diffUrl"`
	FilesURL       string            `json:"filesUrl"`
	Limits         TrailReviewLimits `json:"limits"`
}

// TrailReviewLimits carries the server-enforced batch limits for a review.
type TrailReviewLimits struct {
	MaxCommentsPerBatch int `json:"maxCommentsPerBatch"`
}

// TrailReviewCommentBatchRequest posts a batch of findings to a review via
// POST /api/v1/trails/{trail_id}/reviews/{id}/comments. The API requires at
// least one comment and rejects batches larger than the review's
// max_comments_per_batch limit.
type TrailReviewCommentBatchRequest struct {
	Comments []TrailReviewCommentInput `json:"comments"`
}

// TrailReviewCommentInput is a single finding within a batch create request.
// client_id (an idempotency key) and location are required by the API.
type TrailReviewCommentInput struct {
	ClientID        string                                   `json:"clientId"`
	Body            *string                                  `json:"body,omitempty"`
	Severity        *string                                  `json:"severity,omitempty"`
	Confidence      *float64                                 `json:"confidence,omitempty"`
	Status          *string                                  `json:"status,omitempty"`
	StatusReason    *string                                  `json:"statusReason,omitempty"`
	Location        TrailReviewLocationCreateRequest         `json:"location"`
	SuggestedChange *TrailReviewSuggestedChangeCreateRequest `json:"suggestedChange,omitempty"`
}

// TrailReviewCommentBatchResponse is returned by the batch comment endpoint.
type TrailReviewCommentBatchResponse struct {
	Results []TrailReviewCommentBatchResult `json:"results"`
}

// TrailReviewCommentBatchResult reports the per-finding outcome of a batch.
// Status is one of "created", "existing", or "error"; Comment is populated for
// the first two, Error for the last.
type TrailReviewCommentBatchResult struct {
	ClientID        string                        `json:"clientId"`
	Status          string                        `json:"status"`
	Comment         *TrailReviewComment           `json:"comment,omitempty"`
	SuggestedChange *TrailReviewSuggestedChange   `json:"suggestedChange,omitempty"`
	Error           *TrailReviewCommentBatchError `json:"error,omitempty"`
}

// TrailReviewCommentBatchError describes why a single finding in a batch failed.
type TrailReviewCommentBatchError struct {
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	Field     *string `json:"field"`
	Retryable bool    `json:"retryable"`
}

// TrailReviewLocationCreateRequest identifies where a new finding applies.
type TrailReviewLocationCreateRequest struct {
	Granularity  string  `json:"granularity"`
	FilePath     *string `json:"filePath,omitempty"`
	StartLine    *int    `json:"startLine,omitempty"`
	StartColumn  *int    `json:"startColumn,omitempty"`
	EndLine      *int    `json:"endLine,omitempty"`
	EndColumn    *int    `json:"endColumn,omitempty"`
	SelectedText *string `json:"selectedText,omitempty"`
	NearbyText   *string `json:"nearbyText,omitempty"`
	Language     *string `json:"language,omitempty"`
}

// TrailReviewSuggestedChangeCreateRequest attaches a suggested fix to a new finding.
//
// Every change_type other than manual_instruction requires the full expected_*
// anchor — the API rejects a patch that arrives without it. The anchor describes
// the pre-image the patch was written against, so a later apply can tell whether
// the file has moved on:
//
//   - ExpectedFileHash is the git blob OID of the whole file as it stood in the
//     author's worktree, i.e. what `git hash-object <file>` prints in that repo.
//     The server stores it opaquely, so this is the CLI's convention; keep
//     producers in agreement before relying on it for staleness checks.
//   - ExpectedLines is the byte-exact content of ExpectedStartLine..ExpectedEndLine
//     with line endings intact — not CRLF-normalized display text.
type TrailReviewSuggestedChangeCreateRequest struct {
	ChangeType        string  `json:"changeType"`
	Patch             *string `json:"patch,omitempty"`
	Instruction       *string `json:"instruction,omitempty"`
	ExpectedFilePath  *string `json:"expectedFilePath,omitempty"`
	ExpectedFileHash  *string `json:"expectedFileHash,omitempty"`
	ExpectedStartLine *int    `json:"expectedStartLine,omitempty"`
	ExpectedEndLine   *int    `json:"expectedEndLine,omitempty"`
	ExpectedLines     *string `json:"expectedLines,omitempty"`
}

// TrailReviewLocation identifies where a finding applies.
type TrailReviewLocation struct {
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

// TrailReviewSuggestedChange describes a machine-applicable or manual fix.
type TrailReviewSuggestedChange struct {
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

// TrailReviewOutgoingLink relates two review comments.
type TrailReviewOutgoingLink struct {
	SourceCommentID string `json:"sourceCommentId"`
	TargetCommentID string `json:"targetCommentId"`
	LinkType        string `json:"linkType"`
}

// TrailReviewCommentPatchRequest updates a review finding.
type TrailReviewCommentPatchRequest struct {
	Title        *string  `json:"title,omitempty"`
	Body         *string  `json:"body,omitempty"`
	Severity     *string  `json:"severity,omitempty"`
	Confidence   *float64 `json:"confidence,omitempty"`
	Status       string   `json:"status,omitempty"`
	StatusReason *string  `json:"statusReason,omitempty"`
}
