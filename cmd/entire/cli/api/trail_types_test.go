package api

import (
	"encoding/json"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/trail"
)

// TestTrailResourceDecodesServerURL covers the wire-compatibility matrix for the
// `url` field the API added:
//   - new cli + new api: the field decodes into TrailResource.URL and is used.
//   - old cli + new api: a client struct predating the field ignores the extra
//     key without error (Go's json.Unmarshal drops unknown fields), so an older
//     CLI keeps working against a newer server.
//
// (new cli + old api is exercised by trailDisplayURL's fallback in the cli pkg.)
func TestTrailResourceDecodesServerURL(t *testing.T) {
	t.Parallel()

	// Shape a newer server would emit: includes `url`.
	payload := []byte(`{"id":"t1","number":640,"url":"https://entire.io/gh/o/r/trails/640/slug","branch":"feat/x","title":"T"}`)

	// new cli + new api: URL is captured and available to display.
	var newClient TrailResource
	if err := json.Unmarshal(payload, &newClient); err != nil {
		t.Fatalf("new client failed to decode new payload: %v", err)
	}
	if newClient.URL != "https://entire.io/gh/o/r/trails/640/slug" {
		t.Fatalf("URL = %q, want server-provided url", newClient.URL)
	}

	// old cli + new api: a struct without a URL field must not choke on the
	// extra key, and still decodes the fields it knows about.
	var oldClient struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(payload, &oldClient); err != nil {
		t.Fatalf("old client rejected new payload with extra url field: %v", err)
	}
	if oldClient.Number != 640 || oldClient.Title != "T" {
		t.Fatalf("old client decoded wrong values: %+v", oldClient)
	}
}

func TestTrailResourceToMetadataUsesID(t *testing.T) {
	t.Parallel()

	metadata := (&TrailResource{ID: "trail-db-id", URL: "https://entire.io/gh/o/r/trails/9", Branch: "feature/x", Phase: "has_code"}).ToMetadata()
	if got := metadata.TrailID.String(); got != "trail-db-id" {
		t.Fatalf("metadata TrailID = %q, want stable API id", got)
	}
	if metadata.Phase != "has_code" {
		t.Fatalf("metadata Phase = %q, want has_code", metadata.Phase)
	}
	// The server-provided URL must propagate so callers relying on ToMetadata()
	// don't silently drop it.
	if metadata.URL != "https://entire.io/gh/o/r/trails/9" {
		t.Fatalf("metadata URL = %q, want propagated server url", metadata.URL)
	}
}

func TestToMetadataMapsTypePriorityReviewers(t *testing.T) {
	t.Parallel()
	login := "octocat"
	r := &TrailResource{
		Type:      "bug",
		Priority:  "high",
		Reviewers: []trail.Reviewer{{Login: "rev1", Status: trail.ReviewerApproved}},
		Author:    &trail.Author{ID: "1", Login: &login},
	}
	m := r.ToMetadata()
	if m.Type != trail.TypeBug {
		t.Errorf("Type = %q, want bug", m.Type)
	}
	if m.Priority != trail.PriorityHigh {
		t.Errorf("Priority = %q, want high", m.Priority)
	}
	if len(m.Reviewers) != 1 || m.Reviewers[0].Login != "rev1" {
		t.Errorf("Reviewers = %#v, want one rev1", m.Reviewers)
	}
}

// TestTrailApprovalDecodesStringAuthor pins the wire shape of an approval's
// author. GET .../trails/{forge}/{owner}/{repo}/{number}/approvals sends it as a
// plain GitHub login string:
//
//	{"approvals":[{"id":"59ef5b87","event":"approved","author":"nodo",…}]}
//
// This is deliberately unlike TrailResource.author, which is an
// {"id":…,"login":…} object — so the two cannot share a type. Declaring the
// approval's author as *trail.Author made every populated response fail to
// decode with "cannot unmarshal string into Go struct field
// TrailApproval.approvals.author", which meant `entire trail approvals` could
// only ever print approvals it did not have: an empty list decodes fine, so a
// trail with no approvals looked healthy while an approved trail errored out.
func TestTrailApprovalDecodesStringAuthor(t *testing.T) {
	t.Parallel()

	// Verbatim response body from the production API.
	const body = `{"approvals":[{"id":"59ef5b87","body":null,"event":"approved",` +
		`"author":"nodo","commit_sha":"e9a9dcbf1fbc55580e7212096824a01e1691853d",` +
		`"created_at":"2026-08-11T09:35:11.714Z"}]}`

	var got TrailApprovalsResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding a real approvals response failed: %v", err)
	}
	if len(got.Approvals) != 1 {
		t.Fatalf("Approvals len = %d, want 1", len(got.Approvals))
	}

	a := got.Approvals[0]
	if a.Author != "nodo" {
		t.Errorf("Author = %q, want %q", a.Author, "nodo")
	}
	if a.Event != "approved" {
		t.Errorf("Event = %q, want approved", a.Event)
	}
	if a.CommitSHA != "e9a9dcbf1fbc55580e7212096824a01e1691853d" {
		t.Errorf("CommitSHA = %q", a.CommitSHA)
	}
	// body:null must not become the string "null".
	if a.Body != "" {
		t.Errorf("Body = %q, want empty for a null body", a.Body)
	}
	if a.CreatedAt.IsZero() {
		t.Error("CreatedAt did not decode")
	}
}

// TestTrailApprovalResponseDecodesStringAuthor covers the POST path. It embeds the
// same struct, so `entire trail approve` reported a decode failure *after* the
// server had already recorded the approval.
func TestTrailApprovalResponseDecodesStringAuthor(t *testing.T) {
	t.Parallel()

	const body = `{"ok":true,"approval":{"id":"9f65e574","event":"approved",` +
		`"author":"nodo","created_at":"2026-08-11T09:35:34.998Z"}}`

	var got TrailApprovalResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding a real approve response failed: %v", err)
	}
	if !got.OK {
		t.Error("OK = false, want true")
	}
	if got.Approval.Author != "nodo" {
		t.Errorf("Approval.Author = %q, want nodo", got.Approval.Author)
	}
}
