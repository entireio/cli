package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestBuildApprovalRequestRequiresMessageForRequestChanges(t *testing.T) {
	t.Parallel()
	if _, err := buildApprovalRequest("REQUEST_CHANGES", "  "); err == nil {
		t.Error("REQUEST_CHANGES without message should be rejected")
	}
	req, err := buildApprovalRequest("REQUEST_CHANGES", "please fix")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Event != "REQUEST_CHANGES" || req.Body != "please fix" {
		t.Fatalf("req = %#v", req)
	}
}

func TestBuildApprovalRequestApproveAllowsEmptyMessage(t *testing.T) {
	t.Parallel()
	req, err := buildApprovalRequest("APPROVE", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Event != "APPROVE" || req.Body != "" {
		t.Fatalf("req = %#v", req)
	}
}

func TestTrailApprovalsPath(t *testing.T) {
	t.Parallel()
	got := trailApprovalsPath("gh", "acme", "widgets", 7)
	if !strings.HasSuffix(got, "/7/approvals") {
		t.Fatalf("path = %q, want .../7/approvals suffix", got)
	}
}

func TestTrailApprovalCmdsHaveExpectedFlags(t *testing.T) {
	t.Parallel()
	if newTrailApproveCmd().Flags().Lookup("message") == nil {
		t.Error("approve missing --message")
	}
	if newTrailRequestChangesCmd().Flags().Lookup("message") == nil {
		t.Error("request-changes missing --message")
	}
	if newTrailApprovalsCmd().Flags().Lookup("json") == nil {
		t.Error("approvals missing --json")
	}
}

// TestRenderTrailApprovalsShowsAuthorLogin covers the render path that the
// author-shape mismatch broke. The approvals endpoint sends `"author":"nodo"`, so
// the login must reach the output; when this was decoded as a *trail.Author the
// whole response failed before rendering and the command printed only an error.
func TestRenderTrailApprovalsShowsAuthorLogin(t *testing.T) {
	t.Parallel()

	created, err := time.Parse(time.RFC3339, "2026-08-11T09:35:11Z")
	if err != nil {
		t.Fatalf("parse time: %v", err)
	}
	approvals := []api.TrailApproval{
		{
			ID:        "59ef5b87",
			Author:    "nodo",
			Event:     "approved",
			CommitSHA: "e9a9dcbf1fbc55580e7212096824a01e1691853d",
			CreatedAt: created,
		},
		{
			ID:        "9f65e574",
			Author:    "reviewer2",
			Event:     "changes_requested",
			Body:      "needs a test",
			CommitSHA: "d55dfa6",
			CreatedAt: created,
		},
	}

	var buf bytes.Buffer
	renderTrailApprovals(&buf, approvals)
	out := buf.String()

	for _, want := range []string{"approved", "nodo", "e9a9dcb", "changes_requested", "reviewer2", "needs a test"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The 40-char SHA is abbreviated, so the full hash must not appear.
	if strings.Contains(out, "e9a9dcbf1fbc55580e7212096824a01e1691853d") {
		t.Errorf("commit SHA should be abbreviated:\n%s", out)
	}
}
