package api

import "time"

// ChangeReviewStateResponse is returned by GET /api/v1/changes/{change_id}/reviews/{id}.
type ChangeReviewStateResponse struct {
	Review      ChangeReview            `json:"review"`
	CodeVersion ChangeReviewCodeVersion `json:"codeVersion"`
	Counts      ChangeReviewCounts      `json:"counts"`
	Comments    []ChangeReviewComment   `json:"comments"`
	NextCursor  *string                 `json:"nextCursor"`
	EventCursor string                  `json:"eventCursor"`
}

// ChangeReview represents a review session.
type ChangeReview struct {
	ID            string    `json:"id"`
	ChangeID      string    `json:"changeId"`
	CodeVersionID string    `json:"codeVersionId"`
	ActorID       string    `json:"actorId"`
	Summary       *string   `json:"summary"`
	StartedAt     time.Time `json:"startedAt"`
}

// ChangeReviewCodeVersion pins the base/head that a review covers.
type ChangeReviewCodeVersion struct {
	ID           string    `json:"id"`
	ChangeID     string    `json:"changeId"`
	RepositoryID string    `json:"repositoryId"`
	BaseRef      *string   `json:"baseRef"`
	HeadRef      *string   `json:"headRef"`
	BaseSHA      *string   `json:"baseSha"`
	HeadSHA      *string   `json:"headSha"`
	CapturedAt   time.Time `json:"capturedAt"`
}

// ChangeReviewCounts are review-scoped comment counts.
type ChangeReviewCounts struct {
	Open      int `json:"open"`
	Resolved  int `json:"resolved"`
	Dismissed int `json:"dismissed"`
	Stale     int `json:"stale"`
	Total     int `json:"total"`
}

// ChangeReviewCommentsResponse is returned by change/review comment list endpoints.
type ChangeReviewCommentsResponse struct {
	Comments    []ChangeReviewComment `json:"comments"`
	HasMore     bool                  `json:"hasMore"`
	NextOffset  *int                  `json:"nextOffset"`
	EventCursor string                `json:"eventCursor,omitempty"`
}

// ChangeReviewComment is a single agent-native review finding.
type ChangeReviewComment struct {
	ID                        string                        `json:"id"`
	ChangeID                  string                        `json:"changeId"`
	RepositoryID              string                        `json:"repositoryId"`
	ReviewID                  string                        `json:"reviewId"`
	CodeVersionID             string                        `json:"codeVersionId"`
	ActorID                   string                        `json:"actorId"`
	Title                     *string                       `json:"title"`
	Body                      *string                       `json:"body"`
	Severity                  *string                       `json:"severity"`
	Confidence                *float64                      `json:"confidence"`
	Status                    string                        `json:"status"`
	StatusReason              *string                       `json:"statusReason"`
	StaleOutcome              string                        `json:"staleOutcome"`
	StaleCheckedAt            *time.Time                    `json:"staleCheckedAt"`
	StaleCheckedCodeVersionID *string                       `json:"staleCheckedCodeVersionId"`
	ClientID                  *string                       `json:"clientId"`
	ClientIDHash              *string                       `json:"clientIdHash"`
	CreatedAt                 time.Time                     `json:"createdAt"`
	UpdatedAt                 time.Time                     `json:"updatedAt"`
	Location                  ChangeReviewLocation          `json:"location"`
	SuggestedChanges          []ChangeReviewSuggestedChange `json:"suggestedChanges,omitempty"`
	ThreadID                  *string                       `json:"threadId,omitempty"`
	ThreadMessageCount        int                           `json:"threadMessageCount,omitempty"`
	OutgoingLinks             []ChangeReviewOutgoingLink    `json:"outgoingLinks,omitempty"`
}

// ChangeReviewStartRequest starts a review session for a change via
// POST /api/v1/changes/{change_id}/reviews. All fields are optional; the server
// resolves the code version (base/head) when they are omitted.
type ChangeReviewStartRequest struct {
	HeadSHA *string `json:"headSha,omitempty"`
	BaseSHA *string `json:"baseSha,omitempty"`
	BaseRef *string `json:"baseRef,omitempty"`
	HeadRef *string `json:"headRef,omitempty"`
}

// ChangeReviewStartResponse is returned by POST /api/v1/changes/{change_id}/reviews.
type ChangeReviewStartResponse struct {
	ReviewID       string             `json:"reviewId"`
	ChangeID       string             `json:"changeId"`
	RepositoryID   string             `json:"repositoryId"`
	CodeVersionID  string             `json:"codeVersionId"`
	BaseSHA        *string            `json:"baseSha"`
	HeadSHA        *string            `json:"headSha"`
	EventStreamURL string             `json:"eventStreamUrl"`
	DiffURL        string             `json:"diffUrl"`
	FilesURL       string             `json:"filesUrl"`
	Limits         ChangeReviewLimits `json:"limits"`
}

// ChangeReviewLimits carries the server-enforced batch limits for a review.
type ChangeReviewLimits struct {
	MaxCommentsPerBatch int `json:"maxCommentsPerBatch"`
}

// ChangeReviewCommentBatchRequest posts a batch of findings to a review via
// POST /api/v1/changes/{change_id}/reviews/{id}/comments. The API requires at
// least one comment and rejects batches larger than the review's
// max_comments_per_batch limit.
type ChangeReviewCommentBatchRequest struct {
	Comments []ChangeReviewCommentInput `json:"comments"`
}

// ChangeReviewCommentInput is a single finding within a batch create request.
// client_id (an idempotency key) and location are required by the API.
type ChangeReviewCommentInput struct {
	ClientID        string                                    `json:"clientId"`
	Body            *string                                   `json:"body,omitempty"`
	Severity        *string                                   `json:"severity,omitempty"`
	Confidence      *float64                                  `json:"confidence,omitempty"`
	Status          *string                                   `json:"status,omitempty"`
	StatusReason    *string                                   `json:"statusReason,omitempty"`
	Location        ChangeReviewLocationCreateRequest         `json:"location"`
	SuggestedChange *ChangeReviewSuggestedChangeCreateRequest `json:"suggestedChange,omitempty"`
}

// ChangeReviewCommentBatchResponse is returned by the batch comment endpoint.
type ChangeReviewCommentBatchResponse struct {
	Results []ChangeReviewCommentBatchResult `json:"results"`
}

// ChangeReviewCommentBatchResult reports the per-finding outcome of a batch.
// Status is one of "created", "existing", or "error"; Comment is populated for
// the first two, Error for the last.
type ChangeReviewCommentBatchResult struct {
	ClientID        string                         `json:"clientId"`
	Status          string                         `json:"status"`
	Comment         *ChangeReviewComment           `json:"comment,omitempty"`
	SuggestedChange *ChangeReviewSuggestedChange   `json:"suggestedChange,omitempty"`
	Error           *ChangeReviewCommentBatchError `json:"error,omitempty"`
}

// ChangeReviewCommentBatchError describes why a single finding in a batch failed.
type ChangeReviewCommentBatchError struct {
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	Field     *string `json:"field"`
	Retryable bool    `json:"retryable"`
}

// ChangeReviewLocationCreateRequest identifies where a new finding applies.
type ChangeReviewLocationCreateRequest struct {
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

// ChangeReviewSuggestedChangeCreateRequest attaches a suggested fix to a new finding.
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
type ChangeReviewSuggestedChangeCreateRequest struct {
	ChangeType        string  `json:"changeType"`
	Patch             *string `json:"patch,omitempty"`
	Instruction       *string `json:"instruction,omitempty"`
	ExpectedFilePath  *string `json:"expectedFilePath,omitempty"`
	ExpectedFileHash  *string `json:"expectedFileHash,omitempty"`
	ExpectedStartLine *int    `json:"expectedStartLine,omitempty"`
	ExpectedEndLine   *int    `json:"expectedEndLine,omitempty"`
	ExpectedLines     *string `json:"expectedLines,omitempty"`
}

// ChangeReviewLocation identifies where a finding applies.
type ChangeReviewLocation struct {
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

// ChangeReviewSuggestedChange describes a machine-applicable or manual fix.
type ChangeReviewSuggestedChange struct {
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

// ChangeReviewOutgoingLink relates two review comments.
type ChangeReviewOutgoingLink struct {
	SourceCommentID string `json:"sourceCommentId"`
	TargetCommentID string `json:"targetCommentId"`
	LinkType        string `json:"linkType"`
}

// ChangeReviewCommentPatchRequest updates a review finding.
type ChangeReviewCommentPatchRequest struct {
	Title        *string  `json:"title,omitempty"`
	Body         *string  `json:"body,omitempty"`
	Severity     *string  `json:"severity,omitempty"`
	Confidence   *float64 `json:"confidence,omitempty"`
	Status       string   `json:"status,omitempty"`
	StatusReason *string  `json:"statusReason,omitempty"`
}
