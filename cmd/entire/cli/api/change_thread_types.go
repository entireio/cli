package api

import "time"

// Change discussion-thread wire types. A thread has messages; each message may
// carry a single level of replies. Identity fields differ by source: Author,
// LastMessageAuthor, and Participants[].Login are GitHub logins, while
// CreatedBy and ResolvedBy are actor UUIDs (the server maps them differently).

// ChangeThreadReply is a reply on a thread message. Replies do not nest further.
type ChangeThreadReply struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"` // GitHub login
	CreatedAt time.Time `json:"createdAt"`
	Body      string    `json:"body"`
}

// ChangeThreadMessage is a top-level message in a thread.
type ChangeThreadMessage struct {
	ID        string              `json:"id"`
	Author    string              `json:"author"` // GitHub login
	CreatedAt time.Time           `json:"createdAt"`
	Body      string              `json:"body"`
	Replies   []ChangeThreadReply `json:"replies"`
}

// ChangeThreadParticipant identifies a thread participant by login.
type ChangeThreadParticipant struct {
	Login string `json:"login"`
}

// ChangeThreadSummary is a thread's metadata. The server's review_comment blob
// (present only for kind=="code_review") is intentionally not decoded here:
// code-review threads are surfaced through `change finding`.
type ChangeThreadSummary struct {
	ID                string                    `json:"id"`
	ChangeID          string                    `json:"changeId"`
	Kind              string                    `json:"kind"` // "discussion" | "code_review"
	Title             string                    `json:"title"`
	ReviewCommentID   *string                   `json:"reviewCommentId"`
	Resolved          bool                      `json:"resolved"`
	ResolvedBy        *string                   `json:"resolvedBy"` // actor UUID
	ResolvedAt        *time.Time                `json:"resolvedAt"`
	CreatedBy         *string                   `json:"createdBy"` // actor UUID
	CreatedAt         time.Time                 `json:"createdAt"`
	UpdatedAt         time.Time                 `json:"updatedAt"`
	LastMessageAt     *time.Time                `json:"lastMessageAt"`
	LastMessageAuthor *string                   `json:"lastMessageAuthor"` // GitHub login
	MessageCount      int                       `json:"messageCount"`
	Participants      []ChangeThreadParticipant `json:"participants"`
}

// ChangeThreadsResponse is the response from GET .../:number/threads.
type ChangeThreadsResponse struct {
	Items         []ChangeThreadSummary `json:"items"`
	NextPageToken *string               `json:"nextPageToken,omitempty"`
	EventCursor   string                `json:"eventCursor"`
}

// ChangeThreadDetailResponse is the response from GET .../:number/threads/:id.
type ChangeThreadDetailResponse struct {
	Thread      ChangeThreadSummary   `json:"thread"`
	Messages    []ChangeThreadMessage `json:"messages"`
	EventCursor string                `json:"eventCursor"`
}

// ChangeThreadCreateRequest is the body for POST .../:number/threads.
// Body is required; Title is optional (server defaults it to "Conversation").
type ChangeThreadCreateRequest struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body"`
}

// ChangeThreadCreateResponse is the response from POST .../:number/threads.
type ChangeThreadCreateResponse struct {
	Thread  ChangeThreadSummary  `json:"thread"`
	Message *ChangeThreadMessage `json:"message"`
}

// ChangeThreadUpdateRequest is the body for PATCH .../:number/threads/:id.
// Pointer fields distinguish "not provided" from an explicit value.
type ChangeThreadUpdateRequest struct {
	Title    *string `json:"title,omitempty"`
	Resolved *bool   `json:"resolved,omitempty"`
}

// ChangeThreadUpdateResponse is the response from PATCH .../:number/threads/:id.
type ChangeThreadUpdateResponse struct {
	Thread ChangeThreadSummary `json:"thread"`
}

// ChangeThreadMessageRequest is the body for POST/PATCH message endpoints.
type ChangeThreadMessageRequest struct {
	Body string `json:"body"`
}

// ChangeThreadMessageResponse is the response from the message endpoints.
type ChangeThreadMessageResponse struct {
	Message ChangeThreadMessage `json:"message"`
}
