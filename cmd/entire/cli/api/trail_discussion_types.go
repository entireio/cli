package api

import "time"

// Trail discussion wire types. A discussion has messages; each message may
// carry a single level of replies. Identity fields differ by source: Author,
// LastMessageAuthor, and Participants[].Login are GitHub logins, while
// CreatedBy and ResolvedBy are actor UUIDs (the server maps them differently).

// TrailDiscussionReply is a reply on a discussion message. Replies do not nest further.
type TrailDiscussionReply struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"` // GitHub login
	CreatedAt time.Time `json:"created_at"`
	Body      string    `json:"body"`
}

// TrailDiscussionMessage is a top-level message in a discussion.
type TrailDiscussionMessage struct {
	ID        string                 `json:"id"`
	Author    string                 `json:"author"` // GitHub login
	CreatedAt time.Time              `json:"created_at"`
	Body      string                 `json:"body"`
	Replies   []TrailDiscussionReply `json:"replies"`
}

// TrailDiscussionParticipant identifies a discussion participant by login.
type TrailDiscussionParticipant struct {
	Login string `json:"login"`
}

// TrailDiscussionSummary is a discussion's metadata. The server's review_comment blob
// (present only for kind=="code_review") is intentionally not decoded here:
// code-review discussions are surfaced through `trail finding`.
type TrailDiscussionSummary struct {
	ID                string                       `json:"id"`
	TrailID           string                       `json:"trail_id"`
	Kind              string                       `json:"kind"` // "discussion" | "code_review"
	Title             string                       `json:"title"`
	ReviewCommentID   *string                      `json:"review_comment_id"`
	Resolved          bool                         `json:"resolved"`
	ResolvedBy        *string                      `json:"resolved_by"` // actor UUID
	ResolvedAt        *time.Time                   `json:"resolved_at"`
	CreatedBy         *string                      `json:"created_by"` // actor UUID
	CreatedAt         time.Time                    `json:"created_at"`
	UpdatedAt         time.Time                    `json:"updated_at"`
	LastMessageAt     *time.Time                   `json:"last_message_at"`
	LastMessageAuthor *string                      `json:"last_message_author"` // GitHub login
	MessageCount      int                          `json:"message_count"`
	Participants      []TrailDiscussionParticipant `json:"participants"`
}

// TrailDiscussionsResponse is the response from GET .../:number/discussions.
type TrailDiscussionsResponse struct {
	Items       []TrailDiscussionSummary `json:"items"`
	NextCursor  *string                  `json:"next_cursor,omitempty"`
	EventCursor string                   `json:"event_cursor"`
}

// TrailDiscussionDetailResponse is the response from GET .../:number/discussions/:id.
type TrailDiscussionDetailResponse struct {
	Discussion  TrailDiscussionSummary   `json:"discussion"`
	Messages    []TrailDiscussionMessage `json:"messages"`
	EventCursor string                   `json:"event_cursor"`
}

// TrailDiscussionCreateRequest is the body for POST .../:number/discussions.
// Body is required; Title is optional (server defaults it to "Conversation").
type TrailDiscussionCreateRequest struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// TrailDiscussionCreateResponse is the response from POST .../:number/discussions.
type TrailDiscussionCreateResponse struct {
	Discussion TrailDiscussionSummary  `json:"discussion"`
	Message    *TrailDiscussionMessage `json:"message"`
}

// TrailDiscussionUpdateRequest is the body for PATCH .../:number/discussions/:id.
// Pointer fields distinguish "not provided" from an explicit value.
type TrailDiscussionUpdateRequest struct {
	Title    *string `json:"title,omitempty"`
	Resolved *bool   `json:"resolved,omitempty"`
}

// TrailDiscussionUpdateResponse is the response from PATCH .../:number/discussions/:id.
type TrailDiscussionUpdateResponse struct {
	Discussion TrailDiscussionSummary `json:"discussion"`
}

// TrailDiscussionMessageRequest is the body for POST/PATCH message endpoints.
type TrailDiscussionMessageRequest struct {
	Body string `json:"body"`
}

// TrailDiscussionMessageResponse is the response from the message endpoints.
type TrailDiscussionMessageResponse struct {
	Message TrailDiscussionMessage `json:"message"`
}
