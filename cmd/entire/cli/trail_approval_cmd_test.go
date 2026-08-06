package cli

import (
	"strings"
	"testing"
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
