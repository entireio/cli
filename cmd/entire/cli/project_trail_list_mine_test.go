package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func myTrailTestItem(id string, number int, activity string) map[string]any {
	return map[string]any{"id": id, "number": number, "title": "Trail " + id, "status": "open", "phase": "building",
		"last_activity_at": activity, "created_at": "2026-09-01T12:00:00Z", "metadata_updated_at": "2026-09-02T12:00:00Z",
		"assignee_account_ids": []string{"acct-1"}, "repository_ids": []string{"repo-1"}, "author_account_id": "acct-1",
		"project": map[string]any{"id": "proj-1", "name": "acme", "provider": "github", "jurisdiction": "us",
			"reference": map[string]any{"forge": "gh", "project": "acme"}, "can_manage_trails": true},
		"changes": []any{map[string]any{"id": "chg-1", "repository_id": "repo-1", "repository": "widget", "branch": "feature/x",
			"number": 7, "title": "Widget", "status": "open", "has_code_changes": true,
			"created_at": "2026-09-01T12:00:00Z", "updated_at": activity, "last_activity_at": activity}}}
}

// serveMyTrailsRegion answers /me/project-trails with items, or with status
// when it is not 200, and records each query it was sent.
func serveMyTrailsRegion(t *testing.T, status int, queries *[]string, items ...map[string]any) string {
	t.Helper()
	return serveMyTrailsRegionPage(t, status, queries, nil, items...)
}

// serveMyTrailsRegionPage is serveMyTrailsRegion with the cell's own
// meta.partial reasons.
func serveMyTrailsRegionPage(t *testing.T, status int, queries *[]string, partial []string, items ...map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me/project-trails" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		*queries = append(*queries, r.URL.RawQuery)
		if status != http.StatusOK {
			http.Error(w, `{"error":"region down"}`, status)
			return
		}
		if items == nil {
			items = []map[string]any{}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"items": items, "meta": map[string]any{"partial": partial}}))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func withMyTrailsClusters(t *testing.T, clusters ...coreapi.Cluster) {
	t.Helper()
	withFakeCellCore(t, &fakeCellCore{clusters: clusters})
	withFakeCellClientBuilder(t, &fakeCellClientBuilder{})
}

func myTrailsCluster(slug, jurisdiction, apiURL string, isDefault bool) coreapi.Cluster {
	return coreapi.Cluster{Slug: slug, Jurisdiction: jurisdiction, ApiUrl: coreapi.NewOptString(apiURL), IsDefault: isDefault}
}

func runTrailListMine(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newTrailCmdForMode(true)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"list", "--mine"}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestMyProjectTrailRegions_OneDefaultCellPerJurisdiction(t *testing.T) {
	// Serial: replaces the core client seam.
	withMyTrailsClusters(t,
		myTrailsCluster("eu-secondary", "EU", "https://eu-2.example/", false),
		myTrailsCluster("eu-main", "eu", "https://eu-1.example", true),
		myTrailsCluster("eu-later", "eu", "https://eu-3.example", false),
		myTrailsCluster("us-main", "us", "https://us-1.example", true),
		myTrailsCluster("no-url", "br", "", true),
	)
	regions, err := myProjectTrailRegions(t.Context())
	require.NoError(t, err)
	require.Equal(t, []cellGroup{
		{jurisdiction: "eu", baseURL: "https://eu-1.example"},
		{jurisdiction: "us", baseURL: "https://us-1.example"},
	}, regions)
}

func TestTrailListMine_MergesRegionsByActivity(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var euQueries, usQueries []string
	eu := serveMyTrailsRegion(t, http.StatusOK, &euQueries,
		myTrailTestItem("eu-new", 3, "2026-10-29T12:00:00Z"), myTrailTestItem("eu-old", 1, "2026-10-01T12:00:00Z"))
	us := serveMyTrailsRegion(t, http.StatusOK, &usQueries,
		myTrailTestItem("us-mid", 2, "2026-10-15T12:00:00Z"), myTrailTestItem("eu-new", 3, "2026-10-29T12:00:00Z"))
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", eu, true), myTrailsCluster("us", "us", us, true))

	out, errOut, err := runTrailListMine(t, "--status", "open", "--limit", "2", "--json")
	require.NoError(t, err)
	assert.Empty(t, errOut)
	assert.Equal(t, []string{"per_page=2&sort=updated&status=open"}, euQueries)
	assert.Equal(t, euQueries, usQueries)
	var got struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Partial []string `json:"partial"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 2, "capped at --limit after the merge")
	assert.Equal(t, "eu-new", got.Items[0].ID, "newest first, duplicate dropped")
	assert.Equal(t, "us-mid", got.Items[1].ID)
	assert.Empty(t, got.Partial)
}

func TestTrailListMine_ReportsAFailedRegion(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var euQueries, usQueries []string
	eu := serveMyTrailsRegion(t, http.StatusOK, &euQueries, myTrailTestItem("eu-new", 3, "2026-10-29T12:00:00Z"))
	us := serveMyTrailsRegion(t, http.StatusBadGateway, &usQueries)
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", eu, true), myTrailsCluster("us", "us", us, true))

	out, errOut, err := runTrailListMine(t)
	require.NoError(t, err)
	assert.Regexp(t, `gh/acme\s+3\s+open\s+building\s+2026-10-29T12:00:00Z\s+Trail eu-new`, out)
	assert.Contains(t, errOut, "Warning: list may be incomplete: region us: status 502")

	out, errOut, err = runTrailListMine(t, "--json")
	require.NoError(t, err)
	assert.Contains(t, errOut, "Warning: list may be incomplete: region us: status 502", "warned with --json too")
	var got myProjectTrailsJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, []string{"region us: status 502"}, got.Partial)
}

func TestTrailListMine_FailsWhenEveryRegionFails(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var queries []string
	eu := serveMyTrailsRegion(t, http.StatusInternalServerError, &queries)
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", eu, true))

	out, _, err := runTrailListMine(t)
	require.ErrorContains(t, err, "every region failed")
	assert.Empty(t, out)
}

func TestTrailListMine_RefusesProjectScopedFlags(t *testing.T) {
	// Serial: replaces the core client seam so a stray request would fail loudly.
	old := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return nil, errors.New("unexpected core client") }
	t.Cleanup(func() { newCellCoreClient = old })
	for _, args := range [][]string{{"--project", "gh/acme"}, {"--repo", "gh/acme/widget"}, {"--page-token", "x"}} {
		_, _, err := runTrailListMine(t, args...)
		require.ErrorContains(t, err, "cannot be combined", "args %v", args)
	}
}

func TestTrailListMine_JSONItemsMatchThePlainListShape(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var queries []string
	us := serveMyTrailsRegion(t, http.StatusOK, &queries, myTrailTestItem("us-1", 4, "2026-10-29T12:00:00Z"))
	withMyTrailsClusters(t, myTrailsCluster("us", "us", us, true))

	out, _, err := runTrailListMine(t, "--json")
	require.NoError(t, err)
	var got struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 1)
	item := got.Items[0]
	for _, key := range []string{"id", "projectId", "number", "title", "status", "createdAt", "repoIds", "authorAccountId", "assigneeAccountIds", "project", "changes", "lastActivityAt", "phase"} {
		assert.Contains(t, item, key)
	}
	for _, key := range []string{"created_at", "last_activity_at", "repository_ids"} {
		assert.NotContains(t, item, key, "no cell snake_case keys")
	}
	assert.Equal(t, "proj-1", item["projectId"])
	assert.NotContains(t, item, "updatedAt", "the plain list has no updatedAt")
	changes, ok := item["changes"].([]any)
	require.True(t, ok, "changes is a list")
	require.Len(t, changes, 1)
	change, ok := changes[0].(map[string]any)
	require.True(t, ok, "a change is an object")
	assert.Equal(t, "repo-1", change["repositoryId"])
}

func TestTrailListMine_KeepsTheNewestCopyOfATrail(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var euQueries, usQueries []string
	old := myTrailTestItem("same", 1, "2026-10-01T12:00:00Z")
	old["title"] = "stale copy"
	eu := serveMyTrailsRegion(t, http.StatusOK, &euQueries, old, myTrailTestItem("mid", 2, "2026-10-15T12:00:00Z"))
	us := serveMyTrailsRegion(t, http.StatusOK, &usQueries, myTrailTestItem("same", 1, "2026-10-29T12:00:00Z"))
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", eu, true), myTrailsCluster("us", "us", us, true))

	out, _, err := runTrailListMine(t, "--limit", "1", "--json")
	require.NoError(t, err)
	var got myProjectTrailsJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "same", got.Items[0].ID, "the newer copy sorts first, not the region-order one")
	assert.Equal(t, "Trail same", got.Items[0].Title)
}

func TestTrailListMine_PassesTheCellsPartialReasonsThrough(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var queries []string
	us := serveMyTrailsRegionPage(t, http.StatusOK, &queries, []string{"readiness unavailable"}, myTrailTestItem("us-1", 1, "2026-10-29T12:00:00Z"))
	withMyTrailsClusters(t, myTrailsCluster("us", "us", us, true))

	out, errOut, err := runTrailListMine(t, "--json")
	require.NoError(t, err)
	var got myProjectTrailsJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	assert.Equal(t, []string{"region us: readiness unavailable"}, got.Partial)
	assert.Contains(t, errOut, "region us: readiness unavailable")
}

func TestTrailListMine_AnUnreadableItemMakesItsRegionPartial(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var queries []string
	noActivity := myTrailTestItem("bad-1", 2, "")
	wrongType := myTrailTestItem("bad-2", 3, "2026-10-29T12:00:00Z")
	wrongType["number"] = "three"
	noID := myTrailTestItem("", 4, "2026-10-29T12:00:00Z")
	us := serveMyTrailsRegion(t, http.StatusOK, &queries, myTrailTestItem("good", 1, "2026-10-29T12:00:00Z"), noActivity, wrongType, noID)
	withMyTrailsClusters(t, myTrailsCluster("us", "us", us, true))

	out, _, err := runTrailListMine(t, "--json")
	require.NoError(t, err)
	var got myProjectTrailsJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 1, "the readable item stays")
	assert.Equal(t, "good", got.Items[0].ID)
	assert.Equal(t, []string{"region us: 3 trail(s) could not be read"}, got.Partial, "an item without an id counts too")
}

func TestTrailListMine_ARejectedLoginFailsTheList(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var euQueries, usQueries []string
	eu := serveMyTrailsRegion(t, http.StatusOK, &euQueries, myTrailTestItem("eu-1", 1, "2026-10-29T12:00:00Z"))
	us := serveMyTrailsRegion(t, http.StatusUnauthorized, &usQueries)
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", eu, true), myTrailsCluster("us", "us", us, true))

	out, _, err := runTrailListMine(t, "--json")
	require.ErrorContains(t, err, "region us rejected your login")
	assert.Empty(t, out)
}

func TestTrailListMine_AForbiddenRegionIsPartial(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	var euQueries, usQueries []string
	eu := serveMyTrailsRegion(t, http.StatusOK, &euQueries, myTrailTestItem("eu-1", 1, "2026-10-29T12:00:00Z"))
	us := serveMyTrailsRegion(t, http.StatusForbidden, &usQueries)
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", eu, true), myTrailsCluster("us", "us", us, true))

	out, _, err := runTrailListMine(t, "--json")
	require.NoError(t, err, "a 403 is no access in that region, not a rejected login")
	var got myProjectTrailsJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 1)
	assert.Equal(t, []string{"region us: status 403"}, got.Partial)
}

func TestTrailListMine_RegionFailuresCarryNoResponseBody(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, err := w.Write([]byte("<html><body>" + strings.Repeat("cloudflare ", 300) + "</body></html>"))
		assert.NoError(t, err)
	}))
	t.Cleanup(html.Close)
	var queries []string
	us := serveMyTrailsRegion(t, http.StatusOK, &queries, myTrailTestItem("us-1", 1, "2026-10-29T12:00:00Z"))
	withMyTrailsClusters(t, myTrailsCluster("eu", "eu", html.URL, true), myTrailsCluster("us", "us", us, true))

	out, errOut, err := runTrailListMine(t, "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "cloudflare")
	assert.NotContains(t, errOut, "cloudflare")
	assert.Contains(t, out, `"region eu: status 502"`)
}

func TestTrailListMine_EmptyCatalogAndEmptyList(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	withMyTrailsClusters(t)
	_, _, err := runTrailListMine(t)
	require.ErrorContains(t, err, "names no region with an API URL")

	var queries []string
	us := serveMyTrailsRegion(t, http.StatusOK, &queries)
	withMyTrailsClusters(t, myTrailsCluster("us", "us", us, true))
	out, _, err := runTrailListMine(t)
	require.NoError(t, err)
	assert.Contains(t, out, "No trails authored by or assigned to you.")
}

func TestMergeMyProjectTrails_ALoginFailureForOneRegionFailsTheList(t *testing.T) {
	t.Parallel()
	ok := cellCallResult[myProjectTrailsRegion]{group: cellGroup{jurisdiction: "eu"}}
	for _, authErr := range []error{auth.ErrNotLoggedIn, fmt.Errorf("cell client: %w", auth.ErrReauthRequired)} {
		failed := cellCallResult[myProjectTrailsRegion]{group: cellGroup{jurisdiction: "us"}, err: authErr}
		_, err := mergeMyProjectTrails(t.Context(), []cellCallResult[myProjectTrailsRegion]{ok, failed}, 10)
		require.ErrorIs(t, err, authErr)
	}
}

func TestTrailListMine_ARejectedLoginPrintsNoResponseBody(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write([]byte("<html>\x1b]0;pwned\a" + strings.Repeat("cloudflare ", 300) + "</html>"))
		assert.NoError(t, err)
	}))
	t.Cleanup(page.Close)
	withMyTrailsClusters(t, myTrailsCluster("us", "us", page.URL, true))

	_, _, err := runTrailListMine(t)
	require.ErrorContains(t, err, "region us rejected your login (status 401)")
	assert.NotContains(t, err.Error(), "cloudflare")
	assert.NotContains(t, err.Error(), "\x1b")
	var he *api.HTTPError
	require.ErrorAs(t, err, &he, "the HTTP error stays reachable for callers")
	assert.Equal(t, http.StatusUnauthorized, he.StatusCode)
}

func TestTrailListMine_LeavesOutItemsOutsideTheRequestedStatus(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	closed := myTrailTestItem("closed", 2, "2026-10-30T12:00:00Z")
	closed["status"] = "closed"
	var queries []string
	us := serveMyTrailsRegion(t, http.StatusOK, &queries, myTrailTestItem("open", 1, "2026-10-29T12:00:00Z"), closed)
	withMyTrailsClusters(t, myTrailsCluster("us", "us", us, true))

	out, errOut, err := runTrailListMine(t, "--status", "open", "--json")
	require.NoError(t, err)
	var got struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Partial []string `json:"partial"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "open", got.Items[0].ID)
	assert.Equal(t, []string{"region us: 1 trail(s) did not match status open"}, got.Partial)
	assert.Contains(t, errOut, "did not match status open")
}

func TestTrailListMine_JSONMatchesThePlainList(t *testing.T) {
	// Serial: replaces the core client and cell builder seams.
	item := myTrailTestItem("us-1", 1, "2026-10-29T12:00:00Z")
	delete(item, "metadata_updated_at")
	var queries []string
	us := serveMyTrailsRegion(t, http.StatusOK, &queries, item)
	withMyTrailsClusters(t, myTrailsCluster("us", "us", us, true))

	out, _, err := runTrailListMine(t, "--json")
	require.NoError(t, err)
	assert.NotContains(t, out, "0001-01-01", "no zero times")
	var got struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "2026-10-29T12:00:00Z", got.Items[0]["lastActivityAt"], "the plain list's activity field")
	assert.NotContains(t, got.Items[0], "updatedAt", "the plain list has no updatedAt")
}
