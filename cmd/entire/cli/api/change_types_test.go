package api

import (
	"encoding/json"
	"strings"
	"testing"

	change "github.com/entireio/cli/cmd/entire/cli/trail"
)

// TestChangeResourceDecodesServerURL covers the wire-compatibility matrix for the
// `url` field the API added:
//   - new cli + new api: the field decodes into ChangeResource.URL and is used.
//   - old cli + new api: a client struct predating the field ignores the extra
//     key without error (Go's json.Unmarshal drops unknown fields), so an older
//     CLI keeps working against a newer server.
//
// (new cli + old api is exercised by changeDisplayURL's fallback in the cli pkg.)
func TestChangeResourceDecodesServerURL(t *testing.T) {
	t.Parallel()

	// Shape a newer server would emit: includes `url`.
	payload := []byte(`{"id":"t1","number":640,"url":"https://entire.io/gh/o/r/changes/640/slug","branch":"feat/x","title":"T"}`)

	// new cli + new api: URL is captured and available to display.
	var newClient ChangeResource
	if err := json.Unmarshal(payload, &newClient); err != nil {
		t.Fatalf("new client failed to decode new payload: %v", err)
	}
	if newClient.URL != "https://entire.io/gh/o/r/changes/640/slug" {
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

func TestChangeListResponseDecodesEntireAPIContract(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
		"items":[{
			"id":"01JTRAIL","number":7,"title":"Native change","status":"open",
			"branch":null,"originalBranch":"feature/native","base":"main",
			"requestedReviewers":["reviewer"],"phase":"reviewing",
			"createdAt":"2026-08-10T10:00:00.000Z","updatedAt":"2026-08-10T11:00:00.000Z"
		}],
		"nextPageToken":"cursor-2","totalCount":12
	}`)
	var got ChangeListResponse
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode native list: %v", err)
	}
	if got.Total != 12 || got.NextPageToken == nil || *got.NextPageToken != "cursor-2" {
		t.Fatalf("pagination = total %d token %v", got.Total, got.NextPageToken)
	}
	if len(got.Changes) != 1 || got.Changes[0].Branch != "" || got.Changes[0].OriginalBranch != "feature/native" || got.Changes[0].Phase != "reviewing" {
		t.Fatalf("change = %#v", got.Changes)
	}
}

func TestChangeRequestsUseEntireAPICasing(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(ChangeCreateRequest{Title: "T", BranchName: "feature/x", BranchAction: "link"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{`"branchName"`, `"branchAction"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("request %s missing %s", text, want)
		}
	}
	if strings.Contains(text, "branch_name") || strings.Contains(text, "branch_action") {
		t.Fatalf("request still uses snake_case: %s", text)
	}
}

func TestChangeResourceToMetadataUsesID(t *testing.T) {
	t.Parallel()

	metadata := (&ChangeResource{ID: "change-db-id", URL: "https://entire.io/gh/o/r/changes/9", Branch: "feature/x", Phase: "has_code"}).ToMetadata()
	if got := metadata.TrailID.String(); got != "change-db-id" {
		t.Fatalf("metadata TrailID = %q, want stable API id", got)
	}
	if metadata.Phase != "has_code" {
		t.Fatalf("metadata Phase = %q, want has_code", metadata.Phase)
	}
	// The server-provided URL must propagate so callers relying on ToMetadata()
	// don't silently drop it.
	if metadata.URL != "https://entire.io/gh/o/r/changes/9" {
		t.Fatalf("metadata URL = %q, want propagated server url", metadata.URL)
	}
}

func TestToMetadataMapsTypePriorityReviewers(t *testing.T) {
	t.Parallel()
	login := "octocat"
	r := &ChangeResource{
		Type:      "bug",
		Priority:  "high",
		Reviewers: []change.Reviewer{{Login: "rev1", Status: change.ReviewerApproved}},
		Author:    &change.Author{ID: "1", Login: &login},
	}
	m := r.ToMetadata()
	if m.Type != change.TypeBug {
		t.Errorf("Type = %q, want bug", m.Type)
	}
	if m.Priority != change.PriorityHigh {
		t.Errorf("Priority = %q, want high", m.Priority)
	}
	if len(m.Reviewers) != 1 || m.Reviewers[0].Login != "rev1" {
		t.Errorf("Reviewers = %#v, want one rev1", m.Reviewers)
	}
}

// TestChangeApprovalDecodesStringAuthor pins the current entire-api approvals
// wire shape. Re-verified against entire-api's ChangeApprovalWire: the HTTP
// response uses commitSha/createdAt and a bare login string for author. The
// server's similarly named storedChangeApproval remains snake_case, but is an
// internal JSONB shape that is converted before the response is written.
//
// Author deliberately remains a string rather than *change.Author. A populated
// approvals response otherwise fails to decode even though an empty response
// appears healthy, breaking both `change approvals` and the post-write response
// from `change approve`.
func TestChangeApprovalDecodesStringAuthor(t *testing.T) {
	t.Parallel()

	const body = `{"approvals":[{"id":"59ef5b87","body":null,"event":"approved",` +
		`"author":"nodo","commitSha":"e9a9dcbf1fbc55580e7212096824a01e1691853d",` +
		`"createdAt":"2026-08-11T09:35:11.714Z"}]}`

	var got ChangeApprovalsResponse
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

// The submit response embeds the same camelCase ChangeApprovalWire shape.
func TestChangeApprovalResponseDecodesStringAuthor(t *testing.T) {
	t.Parallel()

	const body = `{"ok":true,"approval":{"id":"9f65e574","event":"approved",` +
		`"author":"nodo","createdAt":"2026-08-11T09:35:34.998Z"}}`

	var got ChangeApprovalResponse
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
