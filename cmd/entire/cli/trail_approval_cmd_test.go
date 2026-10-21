package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestBuildApprovalRequestApproveAllowsEmptyMessage(t *testing.T) {
	t.Parallel()
	req := buildApprovalRequest("approve", "  ")
	if req.Event != "approve" || req.Body != "" {
		t.Fatalf("req = %#v", req)
	}
}

// request-changes was removed; the hidden name points to entire.io for every
// argument shape it used to accept, without any request.
func TestTrailRequestChangesIsRemoved(t *testing.T) {
	t.Parallel()
	for _, project := range []bool{false, true} {
		cmd := newTrailCmdForMode(project)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs([]string{"request-changes", "42", "-m", "Fix it", "--branch", "feature/work"})
		require.ErrorIs(t, cmd.Execute(), errTrailRequestChangesRemoved)
		sub, _, err := cmd.Find([]string{"request-changes"})
		require.NoError(t, err)
		require.True(t, sub.Hidden)
	}
}

func TestTrailApprovalsPath(t *testing.T) {
	t.Parallel()
	const basePath = "/api/v1/repos/native-repo-id/trails"
	got := trailApprovalsPath(basePath, 7)
	if got != basePath+"/7/approvals" {
		t.Fatalf("path = %q, want %q", got, basePath+"/7/approvals")
	}
}

func TestTrailApprovalCmdsHaveExpectedFlags(t *testing.T) {
	t.Parallel()
	if newTrailApproveCmd(legacyTrailMode).Flags().Lookup("message") == nil {
		t.Error("approve missing --message")
	}
	if newTrailApprovalsCmd(legacyTrailMode).Flags().Lookup("json") == nil {
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
