package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// These routing tests replace process-global client constructors.
func TestProjectTrailListSingleCellAndOpaquePagination(t *testing.T) {
	t.Chdir(t.TempDir())
	requests := 0
	const token = "opaque+/=project-page"
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "/api/v1/trails", r.URL.Path)
		want := url.Values{"projectId": {projectTrailTestProject}, "pageSize": {"1"}, "sort": {"updated"}, "groupBy": {"none"}, "status": {"closed"}}
		if requests == 2 {
			want.Set("pageToken", token)
		}
		assert.Equal(t, want, r.URL.Query())
		item := projectTrailTestResource()
		item.Status = "closed"
		// No project, order, or per-item continuation metadata in the new API.
		page := api.ProjectTrailListResponse{Items: []api.ProjectTrail{item}, Jurisdiction: "eu", GroupBy: "none",
			GroupTotals:  []api.ProjectTrailGroupTotal{{Label: "All trails", TotalCount: 2}},
			Capabilities: map[string]bool{"canManageTrails": true}}
		if requests == 1 {
			page.NextPageToken = stringPtr(token)
		}
		assert.NoError(t, json.NewEncoder(w).Encode(page))
	})
	core.clusters = nil // An assigned hidden cell must not require a catalog entry.
	for _, cursor := range []string{"", token} {
		out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--status", "closed", "--limit", "1", "--page-token", cursor, "--json")
		require.NoError(t, err)
		var page api.ProjectTrailListResponse
		require.NoError(t, json.Unmarshal([]byte(out), &page))
		require.Len(t, page.Items, 1)
		require.Equal(t, 2, page.GroupTotals[0].TotalCount)
		require.True(t, page.Capabilities["canManageTrails"])
		if cursor == "" {
			require.Equal(t, token, *page.NextPageToken)
		} else {
			require.Nil(t, page.NextPageToken)
		}
	}
	require.Equal(t, 2, requests, "exactly one cell list read per invocation")
	require.Equal(t, 2, core.resolveCalls)
	require.Zero(t, core.catalogCalls)
}

func TestProjectTrailListRequiresExplicitProject(t *testing.T) {
	for _, inRepo := range []bool{false, true} {
		t.Run(strconv.FormatBool(inRepo), func(t *testing.T) {
			dir := t.TempDir()
			if inRepo {
				testutil.InitRepo(t, dir)
				testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
			}
			t.Chdir(dir)
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("must not read trails: %s", r.URL)
				http.NotFound(w, r)
			})
			for _, args := range [][]string{{"list"}, {"list", "--repo", "gh/acme/widget"}, {"list", "--project", ""}} {
				out, _, err := executeProjectTrailTest(t, args...)
				require.ErrorContains(t, err, "requires --project")
				require.Empty(t, out)
			}
			require.Zero(t, core.resolveCalls)
			require.Zero(t, core.catalogCalls)
		})
	}
}

func TestProjectTrailListRepositoryIsOnlyAFilter(t *testing.T) {
	setupProjectTrailRepoPlacement(t) // Repo's cell differs from the assigned project cell.
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trails", r.URL.Path)
		assert.Equal(t, projectTrailTestProject, r.URL.Query().Get("projectId"))
		assert.Equal(t, "repo-id", r.URL.Query().Get("repoId"))
		item := projectTrailTestResource()
		item.RepositoryIDs = []string{"repo-id"}
		assert.NoError(t, json.NewEncoder(w).Encode(api.ProjectTrailListResponse{Items: []api.ProjectTrail{item}}))
	})
	out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--repo", "gh/acme/widget")
	require.NoError(t, err)
	require.Contains(t, out, "gh/acme")
	require.Contains(t, out, "Project intent")
	require.Equal(t, 1, core.resolveCalls)
	require.Zero(t, core.catalogCalls)
}

func TestProjectTrailListFailuresDoNotFallback(t *testing.T) {
	for _, status := range []int{400, 403, 404, 405, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v1/trails", r.URL.Path)
				w.WriteHeader(status)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"error": "scope or cursor rejected"}))
			})
			out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--page-token", "server-bound-token", "--json")
			require.ErrorContains(t, err, fmt.Sprintf("(status %d)", status))
			require.Empty(t, out)
			require.Equal(t, 1, calls)
			require.Zero(t, core.catalogCalls)
		})
	}
}

func TestProjectTrailListUnavailableProjectDoesNotDiscoverCells(t *testing.T) {
	core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unavailable project must not read trails: %s", r.URL)
		http.NotFound(w, r)
	})
	core.apiURL = ""
	out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--json")
	require.ErrorContains(t, err, "apiUrl")
	require.Empty(t, out)
	require.Zero(t, core.catalogCalls)
}

func TestProjectTrailListRejectsWrongScopeAndStuckCursor(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*api.ProjectTrailListResponse)
		want   string
	}{
		{"wrong project", func(p *api.ProjectTrailListResponse) { p.Items[0].ProjectID = "wrong" }, "identity"},
		{"invalid ID", func(p *api.ProjectTrailListResponse) { p.Items[0].ID = "wrong" }, "identity"},
		{"wrong status", func(p *api.ProjectTrailListResponse) { p.Items[0].Status = "closed" }, "filters"},
		{"oversized page", func(p *api.ProjectTrailListResponse) { p.Items = append(p.Items, p.Items[0]) }, "page size"},
		{"repeated cursor", func(p *api.ProjectTrailListResponse) { p.NextPageToken = stringPtr("same") }, "did not advance"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
				page := api.ProjectTrailListResponse{Items: []api.ProjectTrail{projectTrailTestResource()}}
				tt.mutate(&page)
				assert.NoError(t, json.NewEncoder(w).Encode(page))
			})
			out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--status", "open", "--limit", "1", "--page-token", "same", "--json")
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, out)
		})
	}
}

func TestProjectTrailListValidatesFlagsBeforeIO(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"list", "--limit", "0"}, {"list", "--limit", "101"}, {"list", "--status", "merged"}, {"list", "--project", "gh/acme,gh/other"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			t.Parallel()
			_, _, err := executeProjectTrailTest(t, args...)
			require.Error(t, err)
		})
	}
}

func TestProjectTrailListAgentHelpScope(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "entire"}
	root.AddCommand(newTrailCmdForMode(true))
	cmd, _, err := root.Find([]string{"trail", "list"})
	require.NoError(t, err)
	out := renderAgentHelpCommand(cmd, "gh/acme/widget", true)
	require.Contains(t, out, "Scope: one project")
	require.NotContains(t, out, "Scope: global")
	require.NotContains(t, out, "pass --repo only for a DIFFERENT repo")
}

func TestProjectTrailListEmptyResponse(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, _ *http.Request) {
		assert.NoError(t, json.NewEncoder(w).Encode(api.ProjectTrailListResponse{}))
	})
	out, _, err := executeProjectTrailTest(t, "list", "--project", "gh/acme", "--json")
	require.NoError(t, err)
	require.JSONEq(t, `{"items":[],"nextPageToken":null}`, out)
}
