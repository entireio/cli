package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func intPtr(v int) *int {
	return &v
}

func expertStringPtr(v string) *string {
	return &v
}

func TestExpertsCommandIsHiddenButListedInLabs(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"experts"})
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.True(t, cmd.Hidden)
	require.Contains(t, labsOverview(), "entire experts")
}

func TestRenderExpertsTextShowsEvidence(t *testing.T) {
	resp := expertsResponse{
		RepoFullName: "acme/widget",
		Scopes:       []string{"billing/webhooks/sender.go"},
		Experts: []expertEntry{
			{
				Login:                 "maya",
				Score:                 93,
				MembershipStatus:      "active",
				SessionCount:          2,
				CheckpointCount:       3,
				StepCount:             11,
				AttributionAgentLines: intPtr(120),
				MatchedFiles:          []string{"billing/webhooks/sender.go"},
				Evidence: []expertEvidence{
					{
						SessionID:             "sess-a",
						DisplayName:           "add retry to webhook sender",
						Agent:                 expertStringPtr("codex"),
						LastActivityAt:        "2026-06-20T10:00:00.000Z",
						CheckpointCount:       2,
						StepCount:             9,
						AttributionAgentLines: intPtr(120),
						MatchedFiles:          []string{"billing/webhooks/sender.go"},
					},
				},
			},
		},
	}

	var out bytes.Buffer
	renderExpertsText(&out, resp)

	text := out.String()
	require.Contains(t, text, "EXPERTS for acme/widget")
	require.Contains(t, text, "@maya")
	require.Contains(t, text, "3 checkpoints")
	require.Contains(t, text, "billing/webhooks/sender.go")
	require.Contains(t, text, "add retry to webhook sender")
}

func TestFetchExpertsPostsRequestShape(t *testing.T) {
	var gotPath string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repo_full_name":"acme/widget","scopes":["src/foo.ts"],"query":null,"experts":[],"source":"db"}`))
	}))
	t.Cleanup(srv.Close)

	client := api.NewClientWithBaseURL("tok", srv.URL)
	resp, err := fetchExperts(context.Background(), client, expertsRequest{
		Owner:         "acme",
		Repo:          "widget",
		Scopes:        []string{"src/foo.ts"},
		Branch:        "main",
		Limit:         3,
		EvidenceLimit: 2,
	})

	require.NoError(t, err)
	require.Equal(t, "/api/v1/cache/acme/widget/experts", gotPath)
	require.JSONEq(t, `{"scopes":["src/foo.ts"],"branch":"main","limit":3,"evidence_limit":2}`, gotBody)
	require.Equal(t, "acme/widget", resp.RepoFullName)
}

func TestBuildExpertsRequestNormalizesLocalPathsToRepoRelative(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "cmd/entire/cli/experts_cmd.go", "package cli\n")

	t.Chdir(filepath.Join(repoDir, "cmd", "entire"))
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	req, label, err := buildExpertsRequest(context.Background(), "acme", "widget", expertsCommandOptions{
		Subject: "cli/experts_cmd.go",
		Limit:   5,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"cmd/entire/cli/experts_cmd.go"}, req.Scopes)
	require.Equal(t, "cmd/entire/cli/experts_cmd.go", label)

	req, label, err = buildExpertsRequest(context.Background(), "acme", "widget", expertsCommandOptions{
		Subject: filepath.Join(repoDir, "cmd", "entire", "cli", "experts_cmd.go"),
		Limit:   5,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"cmd/entire/cli/experts_cmd.go"}, req.Scopes)
	require.Equal(t, "cmd/entire/cli/experts_cmd.go", label)
}

func TestBuildExpertsRequestNormalizesLocalDirectoriesToRepoRelativePrefix(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "billing/webhooks/sender.go", "package webhooks\n")

	t.Chdir(filepath.Join(repoDir, "billing"))
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	req, label, err := buildExpertsRequest(context.Background(), "acme", "widget", expertsCommandOptions{
		Subject: "webhooks",
		Limit:   5,
	})

	require.NoError(t, err)
	require.Equal(t, []string{"billing/webhooks/"}, req.Scopes)
	require.Equal(t, "billing/webhooks/", label)
}
