package api

import "time"

// Trail discussion-thread wire types. A thread has messages; each message may
// carry a single level of replies. Identity fields differ by source: Author,
// LastMessageAuthor, and Participants[].Login are GitHub logins, while
// CreatedBy and ResolvedBy are actor UUIDs (the server maps them differently).

// TrailThreadReply is a reply on a thread message. Replies do not nest further.
type TrailThreadReply struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"` // GitHub login
	CreatedAt time.Time `json:"created_at"`
	Body      string    `json:"body"`
}

// TrailThreadMessage is a top-level message in a thread.
type TrailThreadMessage struct {
	ID        string             `json:"id"`
	Author    string             `json:"author"` // GitHub login
	CreatedAt time.Time          `json:"created_at"`
	Body      string             `json:"body"`
	Replies   []TrailThreadReply `json:"replies"`
}

// TrailThreadParticipant identifies a thread participant by login.
type TrailThreadParticipant struct {
	Login string `json:"login"`
}

// TrailThreadSummary is a thread's metadata. The server's review_comment blob
// (present only for kind=="code_review") is intentionally not decoded here:
// code-review threads are surfaced through `trail finding`.
type TrailThreadSummary struct {
	ID                string                   `json:"id"`
	TrailID           string                   `json:"trail_id"`
	Kind              string                   `json:"kind"` // "discussion" | "code_review"
	Title             string                   `json:"title"`
	ReviewCommentID   *string                  `json:"review_comment_id"`
	Resolved          bool                     `json:"resolved"`
	ResolvedBy        *string                  `json:"resolved_by"` // actor UUID
	ResolvedAt        *time.Time               `json:"resolved_at"`
	CreatedBy         *string                  `json:"created_by"` // actor UUID
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
	LastMessageAt     *time.Time               `json:"last_message_at"`
	LastMessageAuthor *string                  `json:"last_message_author"` // GitHub login
	MessageCount      int                      `json:"message_count"`
	Participants      []TrailThreadParticipant `json:"participants"`
}

// TrailThreadsResponse is the response from GET .../:number/threads.
type TrailThreadsResponse struct {
	Items       []TrailThreadSummary `json:"items"`
	EventCursor string               `json:"event_cursor"`
}

// TrailThreadDetailResponse is the response from GET .../:number/threads/:id.
type TrailThreadDetailResponse struct {
	Thread      TrailThreadSummary   `json:"thread"`
	Messages    []TrailThreadMessage `json:"messages"`
	EventCursor string               `json:"event_cursor"`
}

// TrailThreadCreateRequest is the body for POST .../:number/threads.
// Body is required; Title is optional (server defaults it to "Conversation").
type TrailThreadCreateRequest struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// TrailThreadCreateResponse is the response from POST .../:number/threads.
type TrailThreadCreateResponse struct {
	Thread  TrailThreadSummary  `json:"thread"`
	Message *TrailThreadMessage `json:"message"`
}

// TrailThreadUpdateRequest is the body for PATCH .../:number/threads/:id.
// Pointer fields distinguish "not provided" from an explicit value.
type TrailThreadUpdateRequest struct {
	Title    *string `json:"title,omitempty"`
	Resolved *bool   `json:"resolved,omitempty"`
}

// TrailThreadUpdateResponse is the response from PATCH .../:number/threads/:id.
type TrailThreadUpdateResponse struct {
	Thread TrailThreadSummary `json:"thread"`
}

// TrailThreadMessageRequest is the body for POST/PATCH message endpoints.
type TrailThreadMessageRequest struct {
	Body string `json:"body"`
}

// TrailThreadMessageResponse is the response from the message endpoints.
type TrailThreadMessageResponse struct {
	Message TrailThreadMessage `json:"message"`
}
