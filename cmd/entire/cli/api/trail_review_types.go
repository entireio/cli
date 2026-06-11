package api

import "time"

// TrailReviewStateResponse is returned by GET /api/v1/trails/{trail_id}/reviews/{id}.
type TrailReviewStateResponse struct {
	Review      TrailReview            `json:"review"`
	CodeVersion TrailReviewCodeVersion `json:"code_version"`
	Counts      TrailReviewCounts      `json:"counts"`
	Comments    []TrailReviewComment   `json:"comments"`
	NextCursor  *string                `json:"next_cursor"`
	EventCursor string                 `json:"event_cursor"`
}

// TrailReview represents a review session.
type TrailReview struct {
	ID            string    `json:"id"`
	TrailID       string    `json:"trail_id"`
	CodeVersionID string    `json:"code_version_id"`
	ActorID       string    `json:"actor_id"`
	Summary       *string   `json:"summary"`
	StartedAt     time.Time `json:"started_at"`
}

// TrailReviewCodeVersion pins the base/head that a review covers.
type TrailReviewCodeVersion struct {
	ID           string    `json:"id"`
	TrailID      string    `json:"trail_id"`
	RepositoryID string    `json:"repository_id"`
	BaseRef      *string   `json:"base_ref"`
	HeadRef      *string   `json:"head_ref"`
	BaseSHA      *string   `json:"base_sha"`
	HeadSHA      *string   `json:"head_sha"`
	CapturedAt   time.Time `json:"captured_at"`
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
	Comments   []TrailReviewComment `json:"comments"`
	HasMore    bool                 `json:"has_more"`
	NextOffset *int                 `json:"next_offset"`
}

// TrailReviewComment is a single agent-native review finding.
type TrailReviewComment struct {
	ID                        string                       `json:"id"`
	TrailID                   string                       `json:"trail_id"`
	RepositoryID              string                       `json:"repository_id"`
	ReviewID                  string                       `json:"review_id"`
	CodeVersionID             string                       `json:"code_version_id"`
	ActorID                   string                       `json:"actor_id"`
	Title                     *string                      `json:"title"`
	Body                      *string                      `json:"body"`
	Severity                  *string                      `json:"severity"`
	Confidence                *float64                     `json:"confidence"`
	Status                    string                       `json:"status"`
	StatusReason              *string                      `json:"status_reason"`
	StaleOutcome              string                       `json:"stale_outcome"`
	StaleCheckedAt            *time.Time                   `json:"stale_checked_at"`
	StaleCheckedCodeVersionID *string                      `json:"stale_checked_code_version_id"`
	ClientID                  *string                      `json:"client_id"`
	ClientIDHash              *string                      `json:"client_id_hash"`
	CreatedAt                 time.Time                    `json:"created_at"`
	UpdatedAt                 time.Time                    `json:"updated_at"`
	Location                  TrailReviewLocation          `json:"location"`
	SuggestedChanges          []TrailReviewSuggestedChange `json:"suggested_changes,omitempty"`
	ThreadID                  *string                      `json:"thread_id,omitempty"`
	ThreadMessageCount        int                          `json:"thread_message_count,omitempty"`
	OutgoingLinks             []TrailReviewOutgoingLink    `json:"outgoing_links,omitempty"`
}

// TrailReviewCommentCreateRequest creates an agent-native review finding on a trail.
type TrailReviewCommentCreateRequest struct {
	Title            *string                                   `json:"title,omitempty"`
	Body             string                                    `json:"body"`
	Severity         *string                                   `json:"severity,omitempty"`
	Confidence       *float64                                  `json:"confidence,omitempty"`
	ClientID         *string                                   `json:"client_id,omitempty"`
	Location         TrailReviewLocationCreateRequest          `json:"location"`
	SuggestedChanges []TrailReviewSuggestedChangeCreateRequest `json:"suggested_changes,omitempty"`
}

// TrailReviewLocationCreateRequest identifies where a new finding applies.
type TrailReviewLocationCreateRequest struct {
	Granularity  string  `json:"granularity"`
	FilePath     *string `json:"file_path,omitempty"`
	StartLine    *int    `json:"start_line,omitempty"`
	StartColumn  *int    `json:"start_column,omitempty"`
	EndLine      *int    `json:"end_line,omitempty"`
	EndColumn    *int    `json:"end_column,omitempty"`
	SelectedText *string `json:"selected_text,omitempty"`
	NearbyText   *string `json:"nearby_text,omitempty"`
	Language     *string `json:"language,omitempty"`
}

// TrailReviewSuggestedChangeCreateRequest attaches a suggested fix to a new finding.
type TrailReviewSuggestedChangeCreateRequest struct {
	ChangeType        string  `json:"change_type"`
	Patch             *string `json:"patch,omitempty"`
	Instruction       *string `json:"instruction,omitempty"`
	ExpectedFilePath  *string `json:"expected_file_path,omitempty"`
	ExpectedFileHash  *string `json:"expected_file_hash,omitempty"`
	ExpectedStartLine *int    `json:"expected_start_line,omitempty"`
	ExpectedEndLine   *int    `json:"expected_end_line,omitempty"`
	ExpectedLines     *string `json:"expected_lines,omitempty"`
}

// TrailReviewLocation identifies where a finding applies.
type TrailReviewLocation struct {
	ID              string  `json:"id"`
	ReviewCommentID string  `json:"review_comment_id"`
	CodeVersionID   string  `json:"code_version_id"`
	Granularity     string  `json:"granularity"`
	FilePath        *string `json:"file_path"`
	StartLine       *int    `json:"start_line"`
	StartColumn     *int    `json:"start_column"`
	EndLine         *int    `json:"end_line"`
	EndColumn       *int    `json:"end_column"`
	SelectedText    *string `json:"selected_text"`
	NearbyText      *string `json:"nearby_text"`
	Language        *string `json:"language"`
}

// TrailReviewSuggestedChange describes a machine-applicable or manual fix.
type TrailReviewSuggestedChange struct {
	ID                string    `json:"id"`
	ReviewCommentID   string    `json:"review_comment_id"`
	CodeVersionID     string    `json:"code_version_id"`
	ChangeType        string    `json:"change_type"`
	Patch             *string   `json:"patch"`
	Instruction       *string   `json:"instruction"`
	ExpectedFilePath  *string   `json:"expected_file_path"`
	ExpectedFileHash  *string   `json:"expected_file_hash"`
	ExpectedStartLine *int      `json:"expected_start_line"`
	ExpectedEndLine   *int      `json:"expected_end_line"`
	ExpectedLines     *string   `json:"expected_lines"`
	CreatedBy         string    `json:"created_by"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// TrailReviewOutgoingLink relates two review comments.
type TrailReviewOutgoingLink struct {
	SourceCommentID string `json:"source_comment_id"`
	TargetCommentID string `json:"target_comment_id"`
	LinkType        string `json:"link_type"`
}

// TrailReviewCommentPatchRequest updates a review finding.
type TrailReviewCommentPatchRequest struct {
	Title        *string  `json:"title,omitempty"`
	Body         *string  `json:"body,omitempty"`
	Severity     *string  `json:"severity,omitempty"`
	Confidence   *float64 `json:"confidence,omitempty"`
	Status       string   `json:"status,omitempty"`
	StatusReason *string  `json:"status_reason,omitempty"`
}
