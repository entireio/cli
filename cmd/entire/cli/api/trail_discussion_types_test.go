package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const discussionTestLogin = "alice"

func TestTrailDiscussionDetailDecodes(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
	  "discussion": {
	    "id": "th1", "trail_id": "tr1", "kind": "discussion", "title": "Design",
	    "review_comment_id": null, "resolved": false,
	    "resolved_by": null, "resolved_at": null,
	    "created_by": "actor-uuid", "created_at": "2026-07-10T00:00:00Z",
	    "updated_at": "2026-07-10T00:01:00Z",
	    "last_message_at": "2026-07-10T00:01:00Z", "last_message_author": "alice",
	    "message_count": 2, "participants": [{"login":"alice"},{"login":"bob"}]
	  },
	  "messages": [
	    {"id":"m1","author":"alice","created_at":"2026-07-10T00:00:00Z","body":"hi",
	     "replies":[{"id":"r1","author":"bob","created_at":"2026-07-10T00:00:30Z","body":"yo"}]}
	  ],
	  "event_cursor": "42"
	}`)
	var out TrailDiscussionDetailResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.EventCursor != "42" {
		t.Errorf("EventCursor = %q, want 42", out.EventCursor)
	}
	if out.Discussion.CreatedBy == nil || *out.Discussion.CreatedBy != "actor-uuid" {
		t.Errorf("CreatedBy = %v, want actor-uuid", out.Discussion.CreatedBy)
	}
	if out.Discussion.ResolvedBy != nil {
		t.Errorf("ResolvedBy = %v, want nil", out.Discussion.ResolvedBy)
	}
	if out.Discussion.LastMessageAuthor == nil || *out.Discussion.LastMessageAuthor != discussionTestLogin {
		t.Errorf("LastMessageAuthor = %v, want alice", out.Discussion.LastMessageAuthor)
	}
	if len(out.Discussion.Participants) != 2 || out.Discussion.Participants[0].Login != discussionTestLogin {
		t.Errorf("Participants = %#v", out.Discussion.Participants)
	}
	if len(out.Messages) != 1 || out.Messages[0].Author != discussionTestLogin {
		t.Fatalf("Messages = %#v", out.Messages)
	}
	if len(out.Messages[0].Replies) != 1 || out.Messages[0].Replies[0].Author != "bob" {
		t.Errorf("Replies = %#v", out.Messages[0].Replies)
	}
}

func TestTrailDiscussionWriteResponsesDecode(t *testing.T) {
	t.Parallel()
	var created TrailDiscussionCreateResponse
	require.NoError(t, json.Unmarshal([]byte(`{"discussion":{"id":"th1","trail_id":"tr1","kind":"discussion","title":"Thread safety"},"message":{"id":"m1","body":"Keep this thread safe"}}`), &created))
	require.Equal(t, "th1", created.Discussion.ID)
	require.Equal(t, "Thread safety", created.Discussion.Title)
	require.NotNil(t, created.Message)
	require.Equal(t, "Keep this thread safe", created.Message.Body)
	var updated TrailDiscussionUpdateResponse
	require.NoError(t, json.Unmarshal([]byte(`{"discussion":{"id":"th1","kind":"code_review","resolved":true,"review_comment_id":"c1","resolved_by":"actor-example"}}`), &updated))
	require.Equal(t, "th1", updated.Discussion.ID)
	require.Equal(t, "code_review", updated.Discussion.Kind)
	require.True(t, updated.Discussion.Resolved)
	require.Equal(t, "c1", *updated.Discussion.ReviewCommentID)
	require.Equal(t, "actor-example", *updated.Discussion.ResolvedBy)
}
