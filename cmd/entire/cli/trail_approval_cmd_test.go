package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/require"
)

func TestBuildApprovalRequestRequiresMessageForRequestChanges(t *testing.T) {
	t.Parallel()
	if _, err := buildApprovalRequest("request_changes", "  "); err == nil {
		t.Error("request_changes without message should be rejected")
	}
	req, err := buildApprovalRequest("request_changes", "please fix")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Event != "request_changes" || req.Body != "please fix" {
		t.Fatalf("req = %#v", req)
	}
}

func TestBuildApprovalRequestApproveAllowsEmptyMessage(t *testing.T) {
	t.Parallel()
	req, err := buildApprovalRequest("approve", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Event != "approve" || req.Body != "" {
		t.Fatalf("req = %#v", req)
	}
}

func TestTrailApprovalCommandsUseCellEvents(t *testing.T) {
	// Not parallel: replaces the existing client-construction seam.
	for _, target := range []struct{ repo, base string }{
		{"gh/acme/widget", "/api/v1/trails/gh/acme/widget"},
		{"et/acme/widget", "/api/v1/repos/repo_example/trails"},
	} {
		for _, tc := range []struct {
			name, command, message, request, responseEvent, output string
			wantError                                              bool
		}{
			{name: "approve without message", command: "approve", request: `{"event":"approve"}`, responseEvent: "approved", output: "Approved trail #7"},
			{name: "approve with message", command: "approve", message: "  Looks good  ", request: `{"event":"approve","body":"Looks good"}`, responseEvent: "approved", output: "Approved trail #7"},
			{name: "request changes", command: "request-changes", message: "  Please fix  ", request: `{"event":"request_changes","body":"Please fix"}`, responseEvent: "changes_requested", output: "Requested changes on trail #7"},
			{name: "missing reason", command: "request-changes", wantError: true},
			{name: "blank reason", command: "request-changes", message: " \t\n ", wantError: true},
		} {
			t.Run(target.repo+"/"+tc.name, func(t *testing.T) {
				var posts []string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.Method + " " + r.URL.Path {
					case "GET " + target.base + "/7":
						_, _ = fmt.Fprint(w, `{"id":"trail-example","number":7,"status":"open","branch":"feature/example"}`)
					case "POST " + target.base + "/7/approvals":
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						posts = append(posts, string(body))
						_, _ = fmt.Fprintf(w, `{"ok":true,"approval":{"id":"approval-example","event":%q,"author":"reviewer-example","commit_sha":"head-example","created_at":"2026-09-01T00:00:00Z"}}`, tc.responseEvent)
					case "GET " + target.base + "/7/approvals":
						_, _ = fmt.Fprintf(w, `{"approvals":[{"id":"approval-example","event":%q,"author":"reviewer-example","commit_sha":"head-example","created_at":"2026-09-01T00:00:00Z"}]}`, tc.responseEvent)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer srv.Close()
				previous := newTrailAPIClient
				clientCalls := 0
				newTrailAPIClient = func(context.Context, bool, string, string, string) (*api.Client, string, error) {
					clientCalls++
					return api.NewClientWithBaseURL("token", srv.URL), "repo_example", nil
				}
				t.Cleanup(func() { newTrailAPIClient = previous })
				cmd := newTrailCmd()
				cmd.SetContext(t.Context())
				args := []string{tc.command, "7", "--repo", target.repo}
				if tc.message != "" {
					args = append(args, "--message", tc.message)
				}
				cmd.SetArgs(args)
				var out, errOut bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&errOut)
				err := cmd.Execute()
				if tc.wantError {
					require.EqualError(t, err, "--message is required when requesting changes")
					require.Zero(t, clientCalls)
					return
				}
				require.NoError(t, err)
				require.Len(t, posts, 1)
				require.JSONEq(t, tc.request, posts[0])
				require.Equal(t, tc.output+"\n", out.String())
				out.Reset()
				cmd = newTrailCmd()
				cmd.SetContext(t.Context())
				cmd.SetOut(&out)
				cmd.SetErr(&errOut)
				cmd.SetArgs([]string{"approvals", "7", "--repo", target.repo, "--json"})
				require.NoError(t, cmd.Execute())
				var got struct {
					Approvals []struct {
						Event string `json:"event"`
					} `json:"approvals"`
				}
				require.NoError(t, json.Unmarshal(out.Bytes(), &got))
				require.Len(t, got.Approvals, 1)
				require.Equal(t, tc.responseEvent, got.Approvals[0].Event)
			})
		}
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
