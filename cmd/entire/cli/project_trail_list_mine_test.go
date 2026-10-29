package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func myTrailTestItem(id string, number int, activity string) map[string]any {
	return map[string]any{"id": id, "number": number, "title": "Trail " + id, "status": "open", "phase": "building",
		"last_activity_at": activity, "project": map[string]any{"reference": map[string]any{"forge": "gh", "project": "acme"}}}
}

// serveMyTrailsRegion answers /me/project-trails with items, or with status
// when it is not 200, and records each query it was sent.
func serveMyTrailsRegion(t *testing.T, status int, queries *[]string, items ...map[string]any) string {
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
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"items": items, "meta": map[string]any{}}))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func withMyTrailsClusters(t *testing.T, clusters ...coreapi.Cluster) {
	t.Helper()
	old := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return &fakeCellCore{clusters: clusters}, nil }
	t.Cleanup(func() { newCellCoreClient = old })
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
	assert.Contains(t, errOut, "Warning: list may be incomplete: region us:")

	out, _, err = runTrailListMine(t, "--json")
	require.NoError(t, err)
	var got myProjectTrailsJSON
	require.NoError(t, json.Unmarshal([]byte(out), &got), out)
	require.Len(t, got.Partial, 1)
	assert.Contains(t, got.Partial[0], "region us:")
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
