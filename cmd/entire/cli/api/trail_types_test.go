package api

import (
	"encoding/json"
	"strings"
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

func TestTrailListResponseDecodesEntireAPIContract(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
		"items":[{
			"id":"01JTRAIL","number":7,"title":"Native trail","status":"open",
			"branch":null,"original_branch":"feature/native","base":"main",
			"requested_reviewers":["reviewer"],"phase":"reviewing",
			"created_at":"2026-08-10T10:00:00.000Z","updated_at":"2026-08-10T11:00:00.000Z"
		}],
		"next_cursor":"cursor-2","total_count":12
	}`)
	var got TrailListResponse
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode native list: %v", err)
	}
	if got.Total != 12 || got.NextCursor == nil || *got.NextCursor != "cursor-2" {
		t.Fatalf("pagination = total %d token %v", got.Total, got.NextCursor)
	}
	if len(got.Trails) != 1 || got.Trails[0].Branch != "" || got.Trails[0].OriginalBranch != "feature/native" || got.Trails[0].Phase != "reviewing" {
		t.Fatalf("trail = %#v", got.Trails)
	}
}

func TestTrailRequestsUseEntireAPICasing(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(TrailCreateRequest{Title: "T", BranchName: "feature/x", BranchAction: "link"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{`"branch_name"`, `"branch_action"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("request %s missing %s", text, want)
		}
	}
}

func TestTrailResourceToMetadataUsesID(t *testing.T) {
	t.Parallel()

	metadata := (&TrailResource{
		ID:             "trail-db-id",
		URL:            "https://entire.io/gh/o/r/trails/9",
		Branch:         "feature/x",
		OriginalBranch: "feature/former",
		Phase:          "has_code",
	}).ToMetadata()
	if got := metadata.TrailID.String(); got != "trail-db-id" {
		t.Fatalf("metadata TrailID = %q, want stable API id", got)
	}
	if metadata.Phase != "has_code" {
		t.Fatalf("metadata Phase = %q, want has_code", metadata.Phase)
	}
	if metadata.OriginalBranch != "feature/former" {
		t.Fatalf("metadata OriginalBranch = %q, want feature/former", metadata.OriginalBranch)
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

// TestTrailApprovalDecodesStringAuthor pins the current entire-api approvals
// wire shape: snake_case fields and a bare login string for author.
//
// Author deliberately remains a string rather than *trail.Author. A populated
// approvals response otherwise fails to decode even though an empty response
// appears healthy, breaking both `trail approvals` and the post-write response
// from `trail approve`.
func TestTrailApprovalDecodesStringAuthor(t *testing.T) {
	t.Parallel()

	const body = `{"approvals":[{"id":"59ef5b87","body":null,"event":"approved",` +
		`"author":"reviewer-example","commit_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",` +
		`"created_at":"2026-08-11T09:35:11.714Z"}]}`

	var got TrailApprovalsResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding an approvals response failed: %v", err)
	}
	if len(got.Approvals) != 1 {
		t.Fatalf("Approvals len = %d, want 1", len(got.Approvals))
	}

	a := got.Approvals[0]
	if a.Author != "reviewer-example" {
		t.Errorf("Author = %q, want %q", a.Author, "reviewer-example")
	}
	if a.Event != "approved" {
		t.Errorf("Event = %q, want approved", a.Event)
	}
	if a.CommitSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
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

// The submit response embeds the same snake_case TrailApprovalWire shape.
func TestTrailApprovalResponseDecodesStringAuthor(t *testing.T) {
	t.Parallel()

	const body = `{"ok":true,"approval":{"id":"9f65e574","event":"approved",` +
		`"author":"reviewer-example","created_at":"2026-08-11T09:35:34.998Z"}}`

	var got TrailApprovalResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decoding an approve response failed: %v", err)
	}
	if !got.OK {
		t.Error("OK = false, want true")
	}
	if got.Approval.Author != "reviewer-example" {
		t.Errorf("Approval.Author = %q, want reviewer-example", got.Approval.Author)
	}
}

func TestTrailWriteContracts(t *testing.T) {
	t.Parallel()
	reviewers := []string{}
	title := "Renamed"
	no := false
	for _, tc := range []struct {
		name string
		body any
		want string
	}{
		{"update", TrailUpdateRequest{Title: &title, RequestedReviewers: &reviewers}, `{"title":"Renamed","requested_reviewers":[]}`},
		{"clear body", TrailBodyRequest{Markdown: ""}, `{"markdown":""}`},
		{"approve", TrailApprovalRequest{Event: "approve", Body: "Reviewed"}, `{"event":"approve","body":"Reviewed"}`},
		{"request changes", TrailApprovalRequest{Event: "request_changes", Body: "Please fix"}, `{"event":"request_changes","body":"Please fix"}`},
		{"discussion", TrailDiscussionCreateRequest{Title: "Design", Body: "Discuss"}, `{"title":"Design","body":"Discuss"}`},
		{"reopen", TrailDiscussionUpdateRequest{Resolved: &no}, `{"resolved":false}`},
		{"no-op update", TrailDiscussionUpdateRequest{}, `{}`},
		{"message", TrailDiscussionMessageRequest{Body: "Reply"}, `{"body":"Reply"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.want {
				t.Fatalf("body = %s, want %s", body, tc.want)
			}
		})
	}
}
