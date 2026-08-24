package api

import (
	"encoding/json"
	"testing"
)

const threadTestLogin = "alice"

func TestChangeThreadDetailDecodes(t *testing.T) {
	t.Parallel()
	// This payload pins entire-api's delivered wire contract for the change
	// threads endpoint (entireio/entire-api#832) — the only hand-written
	// cross-repo contract fixture in this repo, so don't reshape it without
	// checking that PR's shape first.
	payload := []byte(`{
	  "thread": {
	    "id": "th1", "changeId": "tr1", "kind": "discussion", "title": "Design",
	    "reviewCommentId": null, "resolved": false,
	    "resolvedBy": null, "resolvedAt": null,
	    "createdBy": "actor-uuid", "createdAt": "2026-07-10T00:00:00Z",
	    "updatedAt": "2026-07-10T00:01:00Z",
	    "lastMessageAt": "2026-07-10T00:01:00Z", "lastMessageAuthor": "alice",
	    "messageCount": 2, "participants": [{"login":"alice"},{"login":"bob"}]
	  },
	  "messages": [
	    {"id":"m1","author":"alice","createdAt":"2026-07-10T00:00:00Z","body":"hi",
	     "replies":[{"id":"r1","author":"bob","createdAt":"2026-07-10T00:00:30Z","body":"yo"}]}
	  ],
	  "eventCursor": "42"
	}`)
	var out ChangeThreadDetailResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.EventCursor != "42" {
		t.Errorf("EventCursor = %q, want 42", out.EventCursor)
	}
	if out.Thread.CreatedBy == nil || *out.Thread.CreatedBy != "actor-uuid" {
		t.Errorf("CreatedBy = %v, want actor-uuid", out.Thread.CreatedBy)
	}
	if out.Thread.ResolvedBy != nil {
		t.Errorf("ResolvedBy = %v, want nil", out.Thread.ResolvedBy)
	}
	if out.Thread.LastMessageAuthor == nil || *out.Thread.LastMessageAuthor != threadTestLogin {
		t.Errorf("LastMessageAuthor = %v, want alice", out.Thread.LastMessageAuthor)
	}
	if len(out.Thread.Participants) != 2 || out.Thread.Participants[0].Login != threadTestLogin {
		t.Errorf("Participants = %#v", out.Thread.Participants)
	}
	if len(out.Messages) != 1 || out.Messages[0].Author != threadTestLogin {
		t.Fatalf("Messages = %#v", out.Messages)
	}
	if len(out.Messages[0].Replies) != 1 || out.Messages[0].Replies[0].Author != "bob" {
		t.Errorf("Replies = %#v", out.Messages[0].Replies)
	}
}

func TestChangeThreadUpdateRequestMarshalsResolvedFalse(t *testing.T) {
	t.Parallel()
	f := false
	b, err := json.Marshal(ChangeThreadUpdateRequest{Resolved: &f})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"resolved":false}` {
		t.Errorf("got %s, want {\"resolved\":false}", b)
	}
	// Omitting resolved (nil) must drop the field.
	b2, err := json.Marshal(ChangeThreadUpdateRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b2) != `{}` {
		t.Errorf("got %s, want {}", b2)
	}
}
